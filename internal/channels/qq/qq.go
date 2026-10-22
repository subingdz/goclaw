package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/systemmessages"
)

const pairingDebounce = 60 * time.Second

// Compile-time contracts. The instance loader and the delivery path discover these by type
// switch at runtime, so without the assertions a renamed method would silently turn into
// "QQ inherits no overrides" rather than a build failure.
var (
	_ channels.Channel             = (*Channel)(nil)
	_ channels.BlockReplyChannel   = (*Channel)(nil)
	_ channels.ChatBehaviorChannel = (*Channel)(nil)
)

// Channel is one QQ official bot: a websocket event session plus the REST send path.
type Channel struct {
	*channels.BaseChannel

	api        *api
	gw         *gateway
	dmPolicy   string
	groupPoli  string
	groupAllow []string
	chunkLimit int

	blockReply   *bool
	chatBehavior *config.ChatBehaviorConfig

	// audioMgr serves both directions now: STT for inbound voice, and the TTS chain that turns
	// a reply into a voice attachment. mediaHTTP is the inbound download client; outbound
	// uploads ride c.api, whose host is already pinned. Both stay nil until first use.
	audioMgr  *audio.Manager
	mediaHTTP *http.Client

	startCancel context.CancelFunc
	stopOnce    sync.Once
	stopCh      chan struct{}

	// Passive replies must reference a message we are allowed to answer, and QQ permits
	// that only for a few minutes and a few times per message id. This is an explicit
	// budget rather than "remember the last id", because past the budget the API rejects
	// the send and the text has to go out as an active message instead.
	mu        sync.Mutex
	passives  map[string]*passiveTarget
	seenEvent map[string]struct{}
	seenOrder []string
}

type passiveTarget struct {
	msgID   string
	started time.Time
	used    int
}

// New builds a channel from decoded credentials and instance config.
func New(creds Credentials, ic InstanceConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	api, err := newAPI(ic.APIBase, creds.AppID, creds.AppSecret)
	if err != nil {
		return nil, err
	}

	dmPolicy := ic.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing"
	}
	groupPolicy := ic.GroupPolicy
	if groupPolicy == "" {
		groupPolicy = "open"
	}
	chunkLimit := ic.TextChunkLimit
	if chunkLimit <= 0 {
		chunkLimit = maxTextChunkRunes
	}

	base := channels.NewBaseChannel("qq", msgBus, ic.AllowFrom)
	base.ValidatePolicy(dmPolicy, groupPolicy)

	requireMention := true
	if ic.RequireMention != nil {
		requireMention = *ic.RequireMention
	}
	base.SetRequireMention(requireMention)
	if ic.HistoryLimit > 0 {
		base.SetHistoryLimit(ic.HistoryLimit)
	}

	c := &Channel{
		BaseChannel:  base,
		api:          api,
		dmPolicy:     dmPolicy,
		groupPoli:    groupPolicy,
		groupAllow:   ic.GroupAllowFrom,
		chunkLimit:   chunkLimit,
		blockReply:   ic.BlockReply,
		chatBehavior: ic.ChatBehavior,
		stopCh:       make(chan struct{}),
		passives:     make(map[string]*passiveTarget),
		seenEvent:    make(map[string]struct{}),
	}
	c.SetPairingService(pairingSvc)
	return c, nil
}

// WithAudio attaches the speech-to-text manager. Optional: with no manager, voice
// attachments still reach the pipeline as files and placeholders, they are just not
// transcribed — which is the state of a gateway that never configured an ASR backend.
func (c *Channel) WithAudio(mgr *audio.Manager) *Channel {
	c.audioMgr = mgr
	return c
}

// BlockReplyEnabled returns the per-channel block_reply override (nil = inherit).
func (c *Channel) BlockReplyEnabled() *bool { return c.blockReply }

// ChatBehaviorConfig returns the per-channel chat_behavior override (nil = inherit).
func (c *Channel) ChatBehaviorConfig() *config.ChatBehaviorConfig { return c.chatBehavior }

// Start resolves the websocket endpoint and runs the session in the background. Resolving
// happens here, not inside the loop, so an instance with a bad secret or an unapproved
// intent reports it now instead of flapping forever.
func (c *Channel) Start(ctx context.Context) error {
	c.MarkStarting("connecting to QQ gateway")

	wsURL, err := c.api.gatewayURL(ctx)
	if err != nil {
		kind := channels.ChannelFailureKindNetwork
		retryable := true
		if strings.Contains(err.Error(), "http 401") || strings.Contains(err.Error(), "http 403") {
			kind = channels.ChannelFailureKindAuth
		}
		if strings.Contains(err.Error(), "api_base") {
			kind = channels.ChannelFailureKindConfig
			retryable = false
		}
		c.MarkFailed("gateway lookup failed", err.Error(), kind, retryable)
		return err
	}

	c.gw = &gateway{api: c.api, intents: intentGroupAndC2C, onDispatch: c.handleDispatch, wsURL: wsURL}

	runCtx, cancel := context.WithCancel(context.Background())
	c.startCancel = cancel
	go func() {
		defer cancel()
		if err := c.gw.serve(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("qq: gateway session loop ended", "error", err)
			kind := channels.ChannelFailureKindNetwork
			retryable := true
			var perm *permanentError
			if errors.As(err, &perm) {
				kind = channels.ChannelFailureKindConfig
				retryable = false
			}
			c.MarkFailed("session ended", err.Error(), kind, retryable)
			c.SetRunning(false)
		}
	}()

	c.SetRunning(true)
	c.MarkHealthy("QQ gateway session running")
	return nil
}

// Stop ends the event session.
func (c *Channel) Stop(_ context.Context) error {
	c.stopOnce.Do(func() { close(c.stopCh) })
	if c.startCancel != nil {
		c.startCancel()
	}
	c.SetRunning(false)
	return nil
}

// Send delivers one outbound message to a C2C peer or a group: the text first, then any
// attachments as rich-media messages, with an optional spoken version of the text.
//
// The order is deliberate. The passive reply window and its small per-message budget exist for
// answering the human, so the text keeps the reference and media follows it. A failed
// attachment is announced in the chat rather than dropped quietly, because "here is the chart"
// with no chart reads to the user as a broken agent instead of a failed upload.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	kind, target, err := splitChatID(msg.ChatID)
	if err != nil {
		return err
	}

	content := msg.Content
	files := localMedia(msg.Media)

	// TTS: the reply may come back as a voice attachment, and the [[tts]] markers are
	// stripped either way. A channel that never calls this delivers the markers literally.
	if c.audioMgr != nil && content != "" {
		var voice *bus.MediaAttachment
		content, voice = c.ttsVoiceForText(ctx, content, msg.ChatID, msg.Metadata["is_voice_inbound"] == "true")
		if voice != nil {
			files = append(files, *voice)
		}
	}

	seq := 0
	if strings.TrimSpace(content) != "" {
		chunks := channels.ChunkMarkdown(content, c.chunkLimit)
		for i, chunk := range chunks {
			req := c.newSendRequest(msg.ChatID, chunk, i)
			if err := c.api.send(ctx, kind, target, req); err != nil {
				if req.MsgID != "" && isPassiveFailure(err) {
					// The passive window or budget is what failed. Discard the reference and
					// resend as an active message; the text is still deliverable. msg_seq is
					// kept as the chunk's own number, because QQ uses it to deduplicate — a
					// constant 1 would make every fallback beyond the first chunk look like a
					// repeat of an earlier one.
					c.forgetPassive(msg.ChatID)
					if err2 := c.api.send(ctx, kind, target, sendRequest{
						MsgType: msgTypeText, Content: chunk, MsgSeq: i + 1,
					}); err2 != nil {
						return err2
					}
					continue
				}
				return err
			}
			c.consumePassive(msg.ChatID, req.MsgID != "")
		}
		seq = len(chunks)
	}

	var problems []string
	if len(files) > 0 {
		problems = c.deliverMedia(ctx, kind, target, files, seq+1)
	}
	if len(problems) > 0 {
		note := sendRequest{
			MsgType: msgTypeText,
			Content: "[附件发送失败: " + strings.Join(problems, ", ") + "]",
			MsgSeq:  seq + len(files) + 1,
		}
		if err := c.api.send(ctx, kind, target, note); err != nil {
			slog.Debug("qq: could not report the failed attachments either", "chat_id", msg.ChatID, "error", err)
		}
	}
	if seq == 0 && len(files) > 0 && len(problems) == len(files) {
		return fmt.Errorf("qq: all %d attachment(s) failed to send for chat %s", len(files), msg.ChatID)
	}
	return nil
}

// newSendRequest builds the request for chunk i, attaching a passive reference only while
// one is available for this chat. The first chunk of a reply is the one that consumes the
// msg_id; later chunks are active messages by design, which is also what QQ's per-message
// reply budget forces.
func (c *Channel) newSendRequest(chatID, chunk string, i int) sendRequest {
	req := sendRequest{MsgType: msgTypeText, Content: chunk, MsgSeq: i + 1}
	if i != 0 {
		return req
	}
	if p := c.passiveRef(chatID); p != nil {
		req.MsgID = p.msgID
		req.MsgSeq = p.used + 1
	}
	return req
}

// passiveRef returns a still-usable passive target, or nil when there is none, it expired,
// or its reply budget is spent.
func (c *Channel) passiveRef(chatID string) *passiveTarget {
	c.mu.Lock()
	defer c.mu.Unlock()

	p := c.passives[chatID]
	if p == nil || p.msgID == "" {
		return nil
	}
	if time.Since(p.started) > passiveReplyWindow*time.Second || p.used >= passiveReplyBudget {
		delete(c.passives, chatID)
		return nil
	}
	return p
}

// consumePassive records that a msg_id was used once. Only the first chunk of a reply uses
// the passive reference, so used increments only then; an increment for every chunk would
// exhaust a five-message budget after one two-part answer.
func (c *Channel) consumePassive(chatID string, usedPassive bool) {
	if !usedPassive {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.passives[chatID]; p != nil {
		p.used++
	}
}

func (c *Channel) forgetPassive(chatID string) {
	c.mu.Lock()
	delete(c.passives, chatID)
	c.mu.Unlock()
}

// noteInbound records the message id a reply to this chat may reference.
func (c *Channel) noteInbound(chatID, msgID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.passives[chatID] = &passiveTarget{msgID: msgID, started: time.Now()}
}

// handleDispatch turns one gateway event into an inbound message.
func (c *Channel) handleDispatch(t string, raw json.RawMessage) {
	switch t {
	case "C2C_MESSAGE_CREATE", "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE":
	default:
		slog.Debug("qq: event ignored", "type", t)
		return
	}

	var evt messageEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		slog.Warn("qq: undecodable message event", "type", t, "error", err)
		return
	}
	if c.duplicate(evt.ID) {
		slog.Debug("qq: duplicate event dropped", "event_id", evt.ID)
		return
	}

	sender, err := evt.senderKey()
	if err != nil {
		slog.Warn("qq: dropping message with no attributable sender", "type", t, "event_id", evt.ID, "error", err)
		return
	}
	chatID, err := evt.chatKey()
	if err != nil {
		slog.Warn("qq: dropping message with no chat id", "type", t, "event_id", evt.ID, "error", err)
		return
	}

	ctx := store.WithTenantID(context.Background(), c.TenantID())

	content := strings.TrimSpace(evt.Content)
	mediaNote := ""
	if len(evt.Attachments) > 0 {
		// Inbound media arrives as a QQ CDN URL. Fetching it needs the same file API
		// decision as outbound, so the pipeline at least learns a media was present.
		mediaNote = fmt.Sprintf("[附件 %d 个: %s]", len(evt.Attachments), strings.Join(attachmentKinds(evt.Attachments), ", "))
	}
	if content == "" {
		content = mediaNote
	}
	if content == "" {
		content = "[empty message]"
	}

	peerKind := "direct"
	if evt.isGroup() {
		peerKind = "group"
		if !c.allowGroup(ctx, sender, evt.GroupOpenID) {
			return
		}
		// GROUP_AT is @-addressed; a full-mode message only counts when it mentions
		// someone, which is the conservative reading of require_mention.
		if c.RequireMention() && !evt.addressedToBot(t == "GROUP_AT_MESSAGE_CREATE") {
			slog.Debug("qq: group message without a mention ignored",
				"chat_id", chatID, "event_type", t, "event_id", evt.ID)
			return
		}
	} else if !c.allowDM(ctx, sender, chatID) {
		return
	}

	c.noteInbound(chatID, evt.ID)

	// Attachments are fetched only after the policy gates: a stranger who cannot talk to the
	// bot must not be able to make the gateway pull URLs out of the network.
	files, tags := c.processAttachments(ctx, evt.Attachments)
	if tags != "" {
		if content == "" || content == mediaNote {
			content = tags
		} else {
			content = content + "\n" + tags
		}
	}

	metadata := map[string]string{
		"message_id": evt.ID,
		"platform":   "qq",
		"peer_kind":  peerKind,
	}
	if evt.Author.Username != "" {
		metadata["sender_name"] = evt.Author.Username
	}

	slog.Debug("qq: message received",
		"type", t, "sender_id", sender, "chat_id", chatID, "preview", channels.Truncate(content, 50))

	// The MediaFile variant carries MIME and filename; the path-only variant would drop both,
	// and the image reader decides how to decode from the MIME.
	c.HandleAuthorizedMessageMedia(sender, chatID, content, files, metadata, peerKind)
}

func (c *Channel) allowDM(ctx context.Context, senderID, chatID string) bool {
	switch c.CheckDMPolicy(ctx, senderID, c.dmPolicy) {
	case channels.PolicyAllow:
		return true
	case channels.PolicyNeedsPairing:
		c.sendPairingReply(ctx, senderID, chatID, false)
		return false
	default:
		slog.Debug("qq: dm rejected by policy", "sender_id", senderID, "policy", c.dmPolicy)
		return false
	}
}

// allowGroup applies the group policy and, in full-message mode, the mention requirement.
// GROUP_AT_MESSAGE_CREATE is already an @-addressed event, so requiring a mention there is
// automatic; GROUP_MESSAGE_CREATE is the console-side firehose and is where require_mention
// actually decides anything.
func (c *Channel) allowGroup(ctx context.Context, senderID, groupOpenID string) bool {
	switch c.CheckGroupPolicy(ctx, senderID, groupOpenID, c.groupPoli) {
	case channels.PolicyAllow:
		return true
	case channels.PolicyNeedsPairing:
		c.sendPairingReply(ctx, "group:"+groupOpenID, chatPrefixGroup+groupOpenID, true)
		return false
	case channels.PolicyDeny:
		if c.groupPoli == "allowlist" && c.inGroupAllowList(senderID) {
			return true
		}
		slog.Debug("qq: group message rejected", "sender_id", senderID, "policy", c.groupPoli)
		return false
	default:
		return false
	}
}

func (c *Channel) inGroupAllowList(senderID string) bool {
	raw := strings.TrimPrefix(senderID, senderPrefixGroup)
	for _, g := range c.groupAllow {
		if g == senderID || g == raw {
			return true
		}
	}
	return false
}

// sendPairingReply asks the sender (or the group) to be paired before messages are handled.
func (c *Channel) sendPairingReply(ctx context.Context, senderID, chatID string, isGroup bool) {
	ps := c.PairingService()
	if ps == nil {
		return
	}
	if !c.CanSendPairingNotif(senderID, pairingDebounce) {
		return
	}

	code, err := ps.RequestPairing(ctx, senderID, c.Name(), chatID, "default", nil)
	if err != nil {
		slog.Debug("qq: pairing request failed", "sender_id", senderID, "error", err)
		return
	}

	vars := systemmessages.Vars{"platform": "QQ", "sender_id": senderID, "code": code}
	// A group pairing code is announced to the group so an admin can act on it, and the
	// sender id shown there is the one that has to be approved.
	if isGroup {
		vars["chat_id"] = chatID
	}

	text := c.SystemMessage("", systemmessages.KeyPairingAccountRequired, vars)
	if err := c.Send(ctx, bus.OutboundMessage{Channel: c.Name(), ChatID: chatID, Content: text}); err != nil {
		slog.Warn("qq: pairing reply could not be delivered", "chat_id", chatID, "error", err)
		return
	}
	c.MarkPairingNotifSent(senderID)
	slog.Info("qq: pairing reply sent", "sender_id", senderID, "chat_id", chatID)
}

// duplicate reports whether an event id was already handled. QQ documents at-least-once
// delivery ("相同 msg_id 可能重复推送"), so this is required, not a nicety: without it a
// redelivered message runs the agent twice and the user gets two answers.
func (c *Channel) duplicate(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.seenEvent[id]; seen {
		return true
	}
	c.seenEvent[id] = struct{}{}
	c.seenOrder = append(c.seenOrder, id)
	if len(c.seenOrder) > duplicateRetention {
		evict := c.seenOrder[0]
		c.seenOrder = c.seenOrder[1:]
		delete(c.seenEvent, evict)
	}
	return false
}

func attachmentKinds(atts []attachmentRef) []string {
	kinds := make([]string, 0, len(atts))
	for _, a := range atts {
		switch {
		case strings.Contains(a.ContentType, "image"):
			kinds = append(kinds, "image")
		case strings.Contains(a.ContentType, "video"):
			kinds = append(kinds, "video")
		case a.ContentType != "":
			kinds = append(kinds, a.ContentType)
		default:
			kinds = append(kinds, "file")
		}
	}
	return kinds
}
