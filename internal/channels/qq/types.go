// Package qq implements the QQ Official Bot (QQ 开放平台 机器人) channel, API v2.
//
// Protocol facts this implementation is built on, all from the official wiki
// (https://bot.q.qq.com/wiki/develop/api-v2/), July 2026 revision of the autogen pages:
//
//   - auth:    POST /app/getAppAccessToken {"appId","clientSecret"} → access_token +
//     expires_in (seconds, ≤7200). There is NO refresh token: the docs say a request
//     does not renew a still-valid token, and that a new token is issued only inside
//     the final 60 seconds, during which old and new are both valid.
//   - calls:   Authorization: QQBot {access_token}; errors are {"err_code","message",
//     "trace_id"} and the docs explicitly warn not to judge success by "message".
//   - events:  websocket. GET /gateway → {"url"}. Envelope {id,op,d,s,t}; op 10 Hello
//     carries heartbeat_interval in MILLISECONDS; op 2 Identify carries the token,
//     intents and shard; op 1 heartbeat carries the last received s; op 11 ACK;
//     op 6 Resume carries {token, session_id, seq}; op 7 asks for reconnect; op 9 is
//     invalid session. READY arrives as op 0 dispatch with t="READY".
//   - ids:     one human has a DIFFERENT identifier per scenario — user_openid in C2C,
//     member_openid within a group. Whether member_openid is stable across groups is
//     NOT documented, so nothing here assumes that it is.
//
// Two choices in this file are load-bearing and explained where they are used: chat
// identifiers carry a "c2c:"/"grp:" prefix because the two send endpoints are chosen by
// scenario, not by anything readable in the openid itself; and rich media is uploaded as
// base64 on the conversation-scoped endpoint rather than by URL, because a gateway running
// on a LAN has no address QQ's servers can fetch from (see media_send.go).
package qq

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// Wire opcodes, verbatim from the event-emit page's opcode table.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// IntentGroupAndC2C is GROUP_AND_C2C_EVENT (1<<25), the single intent that carries both
// C2C_MESSAGE_CREATE and GROUP_AT_MESSAGE_CREATE. GROUP_MESSAGE_CREATE (every message in
// the group, @ or not) rides the same bit but is gated by a switch in the developer
// console, not by an intent.
const intentGroupAndC2C = 1 << 25

// msg_type values for the two send endpoints. C2C additionally has 6 (typing state),
// which is not used here.
const (
	msgTypeText        = 0
	msgTypeMarkdown    = 2
	msgTypeInputNotify = 6
	msgTypeMedia       = 7
)

// Passive-reply error codes: the window or the per-message reply budget ran out, or the
// msg_id is no longer usable. Falling back to an active message is the documented way
// through (the quotas for those are in the message-overview page: unverified bots get
// 5/s and 30/min, 20/min per relationship, 1000/day per user or group).
const (
	errPassiveWindow      = 40034128 // 被动回复时间或次数超限
	errPassiveExpired     = 40034005 // 回复消息msg_id已过期
	errPassiveIDExpired   = 304103   // 消息 ID 已过期，不能回复
	errPassiveIDInvalid   = 40034024 // 请求参数 msg_id 无效或越权
	errPassiveQuotaExceed = 40034100 // 主动消息超过频控
)

// passiveReplyWindow and passiveReplyBudget implement the conservative reading of two
// contradictory statements in the current docs: the group send page says 5 minutes and 5
// replies, the C2C send page says "被动消息有效时间 60 分钟，每个消息最多回复 4 次" while
// the msg_id row on that same page says "5 分钟内有效". The shorter window and the
// smaller budget are used, because exceeding the real limit costs a reply (the API
// rejects it) while staying under it costs nothing.
const (
	passiveReplyWindow  = 5 * 60 // seconds, matches the msg_id row on both pages
	passiveReplyBudget  = 4      // per msg_id, the lower of the two documented counts
	maxTextChunkRunes   = 1500   // undocumented per-message limit; conservative default, config-able
	defaultHeartbeatMs  = 45000  // only used if Hello carries no heartbeat_interval
	duplicateRetention  = 64     // inbound event ids remembered to absorb at-least-once redelivery
	accessTokenSkewSecs = 60     // the documented dual-valid overlap when renewing
)

// Chat id prefixes. The kind of a QQ conversation cannot be derived from the identifier
// itself — the two send endpoints are /v2/users/{user_openid}/messages and
// /v2/groups/{group_openid}/messages — and a proactive send (cron, heartbeat, the
// message tool) must know which one to call long after the inbound event that taught us
// is gone from memory. Encoding it in the chat id is the only form that survives a
// restart, a session reload, and a cross-chat forward.
const (
	chatPrefixC2C   = "c2c:"
	chatPrefixGroup = "grp:"
)

// Sender id namespaces, same reasoning: user_openid and member_openid come from
// different namespaces, and a bare string from one must never be honoured as a pairing
// recorded for the other.
const (
	senderPrefixC2C   = "c2c:"
	senderPrefixGroup = "member:"
)

// splitChatID returns the conversation kind and the raw openid a send endpoint needs.
func splitChatID(chatID string) (kind, target string, err error) {
	switch {
	case strings.HasPrefix(chatID, chatPrefixC2C):
		return "c2c", strings.TrimPrefix(chatID, chatPrefixC2C), nil
	case strings.HasPrefix(chatID, chatPrefixGroup):
		return "group", strings.TrimPrefix(chatID, chatPrefixGroup), nil
	default:
		return "", "", fmt.Errorf("qq: chat id %q is missing the %q or %q prefix", chatID, chatPrefixC2C, chatPrefixGroup)
	}
}

// Credentials is the per-instance secret blob stored in channel_instances.credentials.
type Credentials struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// InstanceConfig is the per-instance non-secret blob stored in
// channel_instances.config. Field names and defaults mirror the other channels so the
// web UI and the policy engine treat QQ like Feishu or Telegram.
type InstanceConfig struct {
	APIBase        string                     `json:"api_base,omitempty"` // override for tests or a proxy; used as-is, never loopback-rewritten
	AllowFrom      []string                   `json:"allow_from,omitempty"`
	DMPolicy       string                     `json:"dm_policy,omitempty"`    // "pairing" (default), "open", "allowlist", "disabled"
	GroupPolicy    string                     `json:"group_policy,omitempty"` // "open" (default), "allowlist", "disabled", "pairing"
	GroupAllowFrom []string                   `json:"group_allow_from,omitempty"`
	RequireMention *bool                      `json:"require_mention,omitempty"` // default true; only meaningful in full-message group mode
	TextChunkLimit int                        `json:"text_chunk_limit,omitempty"`
	HistoryLimit   int                        `json:"history_limit,omitempty"`
	BlockReply     *bool                      `json:"block_reply,omitempty"`
	ChatBehavior   *config.ChatBehaviorConfig `json:"chat_behavior,omitempty"`
}

// --- Websocket frames ---

// frame is the {id,op,d,s,t} envelope every gateway message uses. s is only present on
// downstream frames and d stays raw because its shape depends on op/t.
type frame struct {
	ID string          `json:"id"`
	Op int             `json:"op"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	D  json.RawMessage `json:"d"`
}

type helloPayload struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

type identifyPayload struct {
	Token      string         `json:"token"`
	Intents    int64          `json:"intents"`
	Shard      [2]int         `json:"shard"`
	Properties map[string]any `json:"properties,omitempty"`
}

type resumePayload struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

type readyPayload struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"user"`
	Shard []int `json:"shard"`
}

// --- Events ---

// messageEvent is the d payload of C2C_MESSAGE_CREATE, GROUP_AT_MESSAGE_CREATE and
// GROUP_MESSAGE_CREATE. Those three differ only in whether group_openid is set and which
// openid of author carries the sender, so one struct covers all three.
type messageEvent struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	// GroupOpenID is set on group events. C2C events have no chat id at all: the peer
	// IS the sender's user_openid.
	GroupOpenID string          `json:"group_openid"`
	Author      messageAuthor   `json:"author"`
	Mentions    []messageAuthor `json:"mentions"`
	Attachments []attachmentRef `json:"attachments"`
}

type messageAuthor struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Bot        bool   `json:"bot"`
	UserOpenID string `json:"user_openid"`
	// MemberOpenID is "群成员 OpenID（群聊场景使用）". Its stability across groups is not
	// documented, so it is only ever used as (group, member) identity.
	MemberOpenID string `json:"member_openid"`
	UnionOpenID  string `json:"union_openid"`
	MemberRole   string `json:"member_role"`
}

// attachmentRef is MessageAttachment. Kept narrow: this version does not fetch media, but
// an inbound attachment must be visible to the pipeline as "there was media here", which
// is what the reply below tells the user.
type attachmentRef struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

func (e *messageEvent) isGroup() bool { return e.GroupOpenID != "" }

// addressedToBot decides whether a group event is aimed at the robot.
//
// GROUP_AT_MESSAGE_CREATE is addressed by definition. In the console's "接收所有消息"
// firehose (GROUP_MESSAGE_CREATE) the only usable signal is d.mentions: the docs list the
// field but never say whether the bot itself appears in it, so this treats an empty
// mentions array as "not addressed". That is the conservative direction — a missed answer
// is recoverable, an unasked-for public reply in a group is not.
func (e *messageEvent) addressedToBot(isAtEvent bool) bool {
	if isAtEvent {
		return true
	}
	return len(e.Mentions) > 0
}

// senderKey builds the identity used for pairing, allowlists and per-user memory. It is
// a method rather than inline because the fallback order is a security decision: the
// namespaced openid for the scenario, and nothing looser.
func (e *messageEvent) senderKey() (string, error) {
	if e.isGroup() {
		if e.Author.MemberOpenID != "" {
			return senderPrefixGroup + e.Author.MemberOpenID, nil
		}
		// An event with a group but no member openid cannot be attributed to a person.
		// Falling back to author.id would silently merge that person into whatever else
		// shares the id, so the message is dropped instead.
		return "", fmt.Errorf("qq: group event %s carries no member_openid", e.ID)
	}
	if e.Author.UserOpenID != "" {
		return senderPrefixC2C + e.Author.UserOpenID, nil
	}
	if e.Author.ID != "" {
		// Older payloads put the C2C openid in author.id. Same namespace, so this is a
		// width-wise safe fallback rather than a cross-namespace one.
		return senderPrefixC2C + e.Author.ID, nil
	}
	return "", fmt.Errorf("qq: c2c event %s carries no sender openid", e.ID)
}

// chatKey builds the conversation identifier a reply must be addressed to.
func (e *messageEvent) chatKey() (string, error) {
	if e.isGroup() {
		return chatPrefixGroup + e.GroupOpenID, nil
	}
	sender, err := e.senderKey()
	if err != nil {
		return "", err
	}
	return chatPrefixC2C + strings.TrimPrefix(sender, senderPrefixC2C), nil
}

// --- Sending ---

type sendRequest struct {
	MsgType int    `json:"msg_type"`
	Content string `json:"content,omitempty"`
	// Markdown is {"content": ...}; the docs require content to be empty when markdown
	// is used, which the send path enforces.
	Markdown *markdownBody `json:"markdown,omitempty"`
	// Media carries an uploaded file for msg_type 7. The prose calls the field
	// media.file_info and the rendered example on the send page spells it "fileinfo";
	// every other field on these endpoints is snake_case, so file_info is what is sent.
	// A 22006 "消息类型与内容不匹配" on a media send is the signal that this reading is
	// wrong, not that the upload failed.
	Media   *mediaRef `json:"media,omitempty"`
	MsgID   string    `json:"msg_id,omitempty"`
	EventID string    `json:"event_id,omitempty"`
	MsgSeq  int       `json:"msg_seq,omitempty"`
}

// mediaRef references an uploaded rich-media object.
type mediaRef struct {
	FileInfo string `json:"file_info"`
}

// mediaUploadRequest is the body of POST /v2/{users|groups}/{id}/files.
type mediaUploadRequest struct {
	FileType   int    `json:"file_type"`
	FileData   string `json:"file_data,omitempty"` // base64 of the bytes; the no-public-URL path
	URL        string `json:"url,omitempty"`
	FileName   string `json:"file_name,omitempty"`
	UploadID   string `json:"upload_id,omitempty"`
	SrvSendMsg bool   `json:"srv_send_msg"`
}

type mediaUploadResponse struct {
	FileUUID string `json:"file_uuid"`
	FileInfo string `json:"file_info"`
	// TTL is seconds; 0 means the file_info may be kept long-term.
	TTL int    `json:"ttl"`
	ID  string `json:"id"`
}

type markdownBody struct {
	Content string `json:"content"`
}

type gatewayResponse struct {
	URL string `json:"url"`
}

// apiError is the documented error envelope. Judge success by ErrCode only — the wiki
// says "请不要依据 message 来判定一个请求是否失败，message 可能会随时调整".
type apiError struct {
	ErrCode int    `json:"err_code"`
	Code    int    `json:"code"` // legacy shape, still returned by some endpoints
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
	Request string `json:"request_id"`
}

func (e *apiError) code() int {
	if e.ErrCode != 0 {
		return e.ErrCode
	}
	return e.Code
}

func (e *apiError) Error() string {
	return fmt.Sprintf("qq api error %d: %s (trace_id=%s)", e.code(), e.Message, e.TraceID)
}

// isPassiveFailure reports whether sending failed because the passive reply window or
// budget was exhausted — the case where retrying the same text as an active message is
// correct rather than a duplicate.
func isPassiveFailure(err error) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return false
	}
	switch e.code() {
	case errPassiveWindow, errPassiveExpired, errPassiveIDExpired, errPassiveIDInvalid:
		return true
	}
	return false
}
