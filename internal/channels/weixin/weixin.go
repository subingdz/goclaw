package weixin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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

const (
	defaultPollTimeoutMs = 35_000
	pollHeadroomMs       = 5_000
	retryDelay           = 2 * time.Second
	backoffAfterFails    = 3
	backoffDelay         = 30 * time.Second
	sessionPause         = time.Hour
	defaultMaxTextLen    = 4000
	pairingDebounce      = 60 * time.Second
	ticketTTL            = 24 * time.Hour
)

var (
	// ErrNotActivated means the instance exists but nobody has scanned the QR code yet.
	ErrNotActivated = errors.New("weixin: channel is not activated (scan the QR code first)")

	// ErrNoReplyContext is the protocol's central limitation: an iLink bot can only answer
	// a peer that messaged it, so scheduled or bot-initiated output has nowhere to go.
	ErrNoReplyContext = errors.New("weixin: this peer has not messaged the bot, and iLink cannot deliver an unsolicited message")
)

// Channel is one activated WeChat personal account.
type Channel struct {
	*channels.BaseChannel

	cli      *client
	st       *state
	dmPolicy string
	maxLen   int

	irUserID string // ilink_user_id, needed by the typing endpoints

	// Rich media, both directions. cdnBase is the only host a CDN client will contact; transport
	// and the two lazy clients are the seams that make the paths testable without a Tencent
	// endpoint (download and upload need different deadlines, so they cannot share one client).
	cdnBase    *url.URL
	transport  http.RoundTripper
	mediaHTTP  *http.Client
	uploadHTTP *http.Client
	audioMgr   *audio.Manager

	cancel context.CancelFunc

	ticketMu sync.Mutex
	tickets  map[string]ticketEntry
}

type ticketEntry struct {
	ticket string
	until  time.Time
}

// New builds a channel from credentials and instance config. An absent token is not an
// error: the instance exists but has not been scanned yet, which is a normal state for a
// freshly created channel and is reported by Start instead.
func New(creds Credentials, ic InstanceConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	baseURL := ic.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	cli, err := newClient(baseURL, creds.Token, ic.Proxy)
	if err != nil {
		return nil, err
	}
	cdnBase, err := parseCDNBase(ic.CDNBaseURL)
	if err != nil {
		return nil, err
	}

	// Inbound media goes through the same operator proxy as the API: an instance that needs a
	// relay to reach iLink needs it to reach the CDN too.
	var mediaRT http.RoundTripper
	if ic.Proxy != "" {
		mediaRT, err = proxyTransport(ic.Proxy)
		if err != nil {
			return nil, err
		}
	}

	dmPolicy := ic.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing"
	}
	maxLen := ic.TextChunkLimit
	if maxLen <= 0 {
		maxLen = defaultMaxTextLen
	}
	stateDir := ic.StateDir
	if stateDir == "" {
		stateDir = config.ExpandHome("~/.goclaw/data/channels/weixin")
	}

	base := channels.NewBaseChannel("weixin", msgBus, ic.AllowFrom)
	base.ValidatePolicy(dmPolicy, "disabled")

	c := &Channel{
		BaseChannel: base,
		cli:         cli,
		st:          newState(stateDir, baseURL, creds.Token),
		cdnBase:     cdnBase,
		transport:   mediaRT,
		dmPolicy:    dmPolicy,
		maxLen:      maxLen,
		irUserID:    creds.IlinkUserID,
		tickets:     make(map[string]ticketEntry),
	}
	if ic.HistoryLimit > 0 {
		base.SetHistoryLimit(ic.HistoryLimit)
	}
	c.SetPairingService(pairingSvc)
	return c, nil
}

// WithAudio gives the channel access to GoClaw's speech-to-text chain, which is what turns an
// untranscribed WeChat voice note into text. Nil leaves voice as a media attachment without a
// transcript, and the placeholder in the message says so.
func (c *Channel) WithAudio(mgr *audio.Manager) *Channel {
	c.audioMgr = mgr
	return c
}

// Start loads the persisted cursor and begins long-polling.
func (c *Channel) Start(ctx context.Context) error {
	if c.cli.token == "" {
		c.MarkFailed("not activated", ErrNotActivated.Error(), channels.ChannelFailureKindConfig, false)
		return ErrNotActivated
	}
	if err := c.st.load(); err != nil {
		c.MarkFailed("state unreadable", err.Error(), channels.ChannelFailureKindConfig, false)
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.pollLoop(runCtx)

	c.SetRunning(true)
	c.MarkHealthy("long-polling WeChat iLink")
	slog.Info("weixin: channel started", "base_url", c.cli.baseURL, "cursor_restored", c.st.currentCursor() != "")
	return nil
}

// Stop ends the poll loop.
func (c *Channel) Stop(_ context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	c.SetRunning(false)
	return nil
}

// pollLoop is the receive side. It owns no business logic: it fetches, advances the cursor
// only after a batch has been handed over, and parks the session when iLink says the
// session is dead.
func (c *Channel) pollLoop(ctx context.Context) {
	nextTimeoutMs := defaultPollTimeoutMs
	fails := 0

	for {
		if ctx.Err() != nil {
			return
		}
		if left, reason, paused := c.st.paused(); paused {
			slog.Warn("weixin: session paused, waiting", "remaining", left.Round(time.Second), "reason", reason)
			if sleepCtx(ctx, left) {
				return
			}
			continue
		}

		callCtx, cancel := context.WithTimeout(ctx, time.Duration(nextTimeoutMs+pollHeadroomMs)*time.Millisecond)
		resp, err := c.cli.getUpdates(callCtx, getUpdatesReq{GetUpdatesBuf: c.st.currentCursor()})
		cancel()

		if err != nil {
			fails++
			wait := retryDelay
			if fails >= backoffAfterFails {
				fails = 0
				wait = backoffDelay
			}
			slog.Warn("weixin: getupdates failed", "error", err, "consecutive_fails", fails, "retry_in", wait)
			if sleepCtx(ctx, wait) {
				return
			}
			continue
		}

		if resp.failed() {
			if resp.code() == errSessionExpired {
				c.st.pauseSession("iLink reported the session expired (-14); re-scan the QR code to re-activate", sessionPause)
				continue
			}
			// The cursor is deliberately not advanced: the same batch will be fetched
			// again, which is the difference between losing messages and duplicating a
			// handful of them.
			slog.Warn("weixin: getupdates rejected", "code", resp.code(), "errmsg", resp.Errmsg)
			if sleepCtx(ctx, retryDelay) {
				return
			}
			continue
		}

		fails = 0
		if resp.LongpollingTimeoutMs > 0 {
			nextTimeoutMs = resp.LongpollingTimeoutMs
		}
		for _, msg := range resp.Msgs {
			c.handleInbound(msg)
		}
		if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != c.st.currentCursor() {
			if err := c.st.saveCursor(resp.GetUpdatesBuf); err != nil {
				slog.Warn("weixin: cursor not persisted", "error", err)
			}
		}
	}
}

// handleInbound turns one iLink message into a pipeline inbound.
func (c *Channel) handleInbound(msg WeixinMessage) {
	sender := msg.FromUserID
	if sender == "" {
		slog.Debug("weixin: message with no sender dropped", "message_id", msg.MessageID)
		return
	}
	if msg.GroupID != "" {
		// An iLink bot is not a group participant: the protocol does not define how a
		// group message should be answered (the reply reference is per peer), so these
		// are dropped rather than guessed at.
		slog.Debug("weixin: group message ignored (not supported by iLink bots)", "group_id", msg.GroupID)
		return
	}

	ctx := store.WithTenantID(context.Background(), c.TenantID())

	// The reply reference is recorded before the policy decision, because the pairing notice
	// that an unknown sender triggers is itself a reply to this message: storing it afterwards
	// meant the first contact could never be answered.
	if msg.ContextToken != "" {
		if err := c.st.rememberToken(sender, msg.ContextToken); err != nil {
			slog.Warn("weixin: reply reference not persisted", "error", err)
		}
	}

	if !c.allowDM(ctx, sender) {
		return
	}

	// Attachments are fetched only for a sender the policy let through: a CDN download is an
	// outbound request made on behalf of whoever messaged, and it should not happen for a
	// rejected one.
	var (
		files   []bus.MediaFile
		tags    string
		handled map[int]bool
	)
	if refs := c.selectInboundMedia(msg.ItemList); len(refs) > 0 {
		mediaCtx, cancel := context.WithTimeout(ctx, weixinMediaBudget)
		files, tags, handled = c.processInboundMedia(mediaCtx, refs)
		cancel()
	}

	content := renderItems(msg.ItemList, handled)
	if tags != "" {
		content = strings.TrimSpace(content + "\n" + tags)
	}
	if content == "" {
		slog.Debug("weixin: message with no usable content dropped", "message_id", msg.MessageID)
		return
	}

	metadata := map[string]string{"platform": "weixin", "peer_kind": "direct"}
	if msg.ClientID != "" {
		metadata["message_id"] = msg.ClientID
	}
	if msg.SessionID != "" {
		metadata["session_id"] = msg.SessionID
	}

	slog.Debug("weixin: message received", "sender_id", sender, "preview", channels.Truncate(content, 50), "attachments", len(files))
	c.HandleAuthorizedMessageMedia(sender, sender, content, files, metadata, "direct")
}

func (c *Channel) allowDM(ctx context.Context, senderID string) bool {
	switch c.CheckDMPolicy(ctx, senderID, c.dmPolicy) {
	case channels.PolicyAllow:
		return true
	case channels.PolicyNeedsPairing:
		c.sendPairingReply(ctx, senderID)
		return false
	default:
		slog.Debug("weixin: message rejected by policy", "sender_id", senderID, "policy", c.dmPolicy)
		return false
	}
}

func (c *Channel) sendPairingReply(ctx context.Context, senderID string) {
	ps := c.PairingService()
	if ps == nil {
		return
	}
	if !c.CanSendPairingNotif(senderID, pairingDebounce) {
		return
	}
	code, err := ps.RequestPairing(ctx, senderID, c.Name(), senderID, "default", nil)
	if err != nil {
		slog.Debug("weixin: pairing request failed", "sender_id", senderID, "error", err)
		return
	}
	text := c.SystemMessage("", systemmessages.KeyPairingAccountRequired, systemmessages.Vars{
		"platform":  "微信",
		"sender_id": senderID,
		"code":      code,
	})
	// The pairing notice is itself a reply: the peer just messaged, so a fresh context
	// token exists and was stored by handleInbound before this call.
	if err := c.Send(ctx, bus.OutboundMessage{Channel: c.Name(), ChatID: senderID, Content: text}); err != nil {
		slog.Warn("weixin: pairing reply could not be delivered", "sender_id", senderID, "error", err)
		return
	}
	c.MarkPairingNotifSent(senderID)
	slog.Info("weixin: pairing reply sent", "sender_id", senderID)
}

// Send delivers one outbound message. Every send is a reply: without a live context token
// for this peer the message is undeliverable, and that is reported rather than swallowed —
// otherwise a cron reminder looks like it was sent when nothing reached WeChat.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("weixin: channel is not running")
	}
	content := msg.Content
	// Copied rather than aliased: appending the TTS file below must not write through the
	// caller's backing array, and OutboundMessage can still be held by the dispatcher.
	files := append([]bus.MediaAttachment(nil), msg.Media...)

	// TTS: the reply may come back as a spoken file, and the [[tts]] markers are stripped
	// either way. A channel that never calls this leaks the markers into the chat.
	if c.audioMgr != nil && content != "" {
		var voice *bus.MediaAttachment
		content, voice = c.ttsVoiceForText(ctx, content, msg.ChatID, msg.Metadata["is_voice_inbound"] == "true")
		if voice != nil {
			files = append(files, *voice)
		}
	}

	token, err := c.st.replyToken(msg.ChatID)
	if err != nil {
		if errors.Is(err, ErrNoReplyContext) || strings.Contains(err.Error(), "expired") {
			slog.Warn("weixin: message not deliverable", "chat_id", msg.ChatID, "error", err)
		}
		return err
	}

	var chunks []string
	if strings.TrimSpace(content) != "" {
		chunks = channels.ChunkMarkdown(content, c.maxLen)
	}
	for _, chunk := range chunks {
		if err := c.sendText(ctx, msg.ChatID, chunk, token); err != nil {
			return err
		}
	}

	var problems []string
	if len(files) > 0 {
		mediaCtx, cancel := context.WithTimeout(ctx, weixinMediaBudget)
		problems = c.sendOutboundMedia(mediaCtx, msg.ChatID, files, token)
		cancel()
	}
	if len(problems) > 0 {
		note := "[附件发送失败: " + strings.Join(problems, ", ") + "]"
		if err := c.sendText(ctx, msg.ChatID, note, token); err != nil {
			slog.Debug("weixin: could not report the failed attachments either", "chat_id", msg.ChatID, "error", err)
		}
	}
	// Text that arrived is text the user got. Only a reply made purely of attachments that all
	// failed is worth failing the send for, so the pipeline can report it itself.
	if len(chunks) == 0 && len(files) > 0 && len(problems) == len(files) {
		return fmt.Errorf("weixin: all %d attachment(s) failed to send for chat %s", len(files), msg.ChatID)
	}
	c.cancelTyping(ctx, msg.ChatID)
	return nil
}

func (c *Channel) sendText(ctx context.Context, to, text, token string) error {
	req := sendMessageReq{Msg: WeixinMessage{
		ToUserID:     to,
		ClientID:     newClientID(),
		MessageType:  2, // bot
		MessageState: 2, // finished
		ItemList:     []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: text}}},
		ContextToken: token,
	}}

	resp, err := c.cli.sendMessage(ctx, req)
	if err != nil {
		return err
	}
	if resp.failed() {
		if resp.code() == errSessionExpired {
			c.st.pauseSession("iLink reported the session expired (-14); re-scan the QR code to re-activate", sessionPause)
		}
		return fmt.Errorf("weixin: sendmessage rejected (code=%d, errmsg=%s)", resp.code(), resp.Errmsg)
	}
	return nil
}

// OnActivityEvent drives WeChat's native "输入中" indicator. Best-effort by contract: any
// failure here is logged at debug and ignored, because the indicator is cosmetic and must
// never affect the real reply.
func (c *Channel) OnActivityEvent(ctx context.Context, chatID string, statusCode string) error {
	if statusCode == "" {
		return nil
	}
	ticket, err := c.typingTicket(ctx, chatID)
	if err != nil || ticket == "" {
		slog.Debug("weixin: typing ticket unavailable", "chat_id", chatID, "error", err)
		return nil
	}
	if _, err := c.cli.sendTyping(ctx, sendTypingReq{
		IlinkUserID: c.irUserID, TypingTicket: ticket, Status: typingStatusTyping,
	}); err != nil {
		slog.Debug("weixin: sendtyping failed", "chat_id", chatID, "error", err)
	}
	return nil
}

func (c *Channel) cancelTyping(ctx context.Context, chatID string) {
	ticket, err := c.typingTicket(ctx, chatID)
	if err != nil || ticket == "" {
		return
	}
	if _, err := c.cli.sendTyping(ctx, sendTypingReq{
		IlinkUserID: c.irUserID, TypingTicket: ticket, Status: typingStatusCancel,
	}); err != nil {
		slog.Debug("weixin: typing cancel failed", "chat_id", chatID, "error", err)
	}
}

// typingTicket fetches and caches the per-conversation typing ticket. getconfig needs a
// context token, so a ticket can only exist for a peer that has messaged.
func (c *Channel) typingTicket(ctx context.Context, chatID string) (string, error) {
	c.ticketMu.Lock()
	if e, ok := c.tickets[chatID]; ok && time.Now().Before(e.until) && e.ticket != "" {
		c.ticketMu.Unlock()
		return e.ticket, nil
	}
	c.ticketMu.Unlock()

	token, err := c.st.replyToken(chatID)
	if err != nil {
		return "", err
	}
	resp, err := c.cli.getConfig(ctx, getConfigReq{IlinkUserID: c.irUserID, ContextToken: token})
	if err != nil {
		return "", err
	}
	if resp.failed() || resp.TypingTicket == "" {
		return "", fmt.Errorf("weixin: no typing ticket (code=%d, errmsg=%s)", resp.code(), resp.Errmsg)
	}
	c.ticketMu.Lock()
	c.tickets[chatID] = ticketEntry{ticket: resp.TypingTicket, until: time.Now().Add(ticketTTL)}
	c.ticketMu.Unlock()
	return resp.TypingTicket, nil
}

func newClientID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "goclaw"
	}
	return "goclaw-" + hex.EncodeToString(b[:])
}

// sleepCtx waits for d unless the context is cancelled first; true means "stop".
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

var (
	_ channels.Channel                  = (*Channel)(nil)
	_ channels.ActivityIndicatorChannel = (*Channel)(nil)
)

// Credentials is the per-instance secret blob. Token is what the QR scan produces.
type Credentials struct {
	Token       string `json:"token"`
	IlinkBotID  string `json:"ilink_bot_id,omitempty"`
	IlinkUserID string `json:"ilink_user_id,omitempty"`
}

// InstanceConfig is the per-instance non-secret blob.
type InstanceConfig struct {
	BaseURL        string   `json:"base_url,omitempty"`
	CDNBaseURL     string   `json:"cdn_base_url,omitempty"` // media CDN host, both directions; pinned, https unless loopback
	Proxy          string   `json:"proxy,omitempty"`
	StateDir       string   `json:"state_dir,omitempty"`
	AllowFrom      []string `json:"allow_from,omitempty"`
	DMPolicy       string   `json:"dm_policy,omitempty"`
	TextChunkLimit int      `json:"text_chunk_limit,omitempty"`
	HistoryLimit   int      `json:"history_limit,omitempty"`
}
