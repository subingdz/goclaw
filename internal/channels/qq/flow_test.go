package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// fakeREST serves the token endpoint and records every send request, so the outbound path
// (passive reply, fallback to active, chunk numbering) can be asserted without a bot.
type fakeREST struct {
	t       *testing.T
	mu      sync.Mutex
	sends   []sendRequest
	paths   []string
	failers map[int]int // err_code to return for the Nth send (1-based)
}

func newFakeREST(t *testing.T) (*fakeREST, *api) {
	t.Helper()
	f := &fakeREST{t: t, failers: map[int]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 7200})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		n := len(f.paths)
		f.mu.Unlock()

		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode send: %v", err)
		}
		f.mu.Lock()
		f.sends = append(f.sends, req)
		code := f.failers[n]
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			// Only err_code decides success here; the message text is deliberately
			// meaningless, because the docs forbid judging by it.
			_, _ = w.Write([]byte(`{"err_code":` + strconv.Itoa(code) + `,"message":"文档里的文案","trace_id":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"err_code":0,"id":"SENT1"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, err := newAPI(srv.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	return f, a
}

func (f *fakeREST) recorded() ([]sendRequest, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendRequest(nil), f.sends...), append([]string(nil), f.paths...)
}

func channelWithAPI(t *testing.T, a *api, ic InstanceConfig) *Channel {
	t.Helper()
	c, err := New(Credentials{AppID: "app", AppSecret: "secret"}, ic, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.api = a
	return c
}

func TestSendUsesGroupEndpointAndPassiveReference(t *testing.T) {
	f, a := newFakeREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	chat := chatPrefixGroup + "G7"
	c.noteInbound(chat, "MID7")

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: chat, Content: "hello"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	sends, paths := f.recorded()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(sends))
	}
	if paths[0] != "/v2/groups/G7/messages" {
		t.Fatalf("path = %q, want the group send endpoint", paths[0])
	}
	if sends[0].MsgID != "MID7" || sends[0].MsgType != msgTypeText || sends[0].Content != "hello" {
		t.Fatalf("send = %+v, want a passive text reply to MID7", sends[0])
	}
}

// TestSendFallsBackToActiveMessage pins the recovery path: when QQ says the passive window
// is gone, the same text still has to reach the user, and it must not look like a duplicate.
func TestSendFallsBackToActiveMessage(t *testing.T) {
	f, a := newFakeREST(t)
	f.mu.Lock()
	f.failers[1] = errPassiveWindow
	f.mu.Unlock()

	c := channelWithAPI(t, a, InstanceConfig{})
	chat := chatPrefixC2C + "U9"
	c.noteInbound(chat, "MID9")

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: chat, Content: "hi"}); err != nil {
		t.Fatalf("Send() error = %v, want the fallback to succeed", err)
	}

	sends, paths := f.recorded()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want a passive attempt plus an active retry", len(sends))
	}
	if sends[0].MsgID != "MID9" {
		t.Fatalf("first attempt = %+v, want it to carry the msg_id", sends[0])
	}
	if sends[1].MsgID != "" {
		t.Fatalf("retry = %+v, want an active message with no msg_id", sends[1])
	}
	if sends[1].MsgSeq != 1 {
		t.Fatalf("retry msg_seq = %d, want the chunk's own sequence", sends[1].MsgSeq)
	}
	if paths[0] != "/v2/users/U9/messages" {
		t.Fatalf("path = %q, want the C2C send endpoint", paths[0])
	}
	// The dead reference must be forgotten, or the next answer would fail the same way.
	if p := c.passiveRef(chat); p != nil {
		t.Fatalf("passiveRef() after fallback = %+v, want it cleared", p)
	}
}

func TestSendPropagatesHardFailure(t *testing.T) {
	f, a := newFakeREST(t)
	f.mu.Lock()
	f.failers[1] = errPassiveQuotaExceed // active quota: no fallback exists for this
	f.mu.Unlock()

	c := channelWithAPI(t, a, InstanceConfig{})
	err := c.Send(context.Background(), bus.OutboundMessage{ChatID: chatPrefixC2C + "U9", Content: "hi"})
	if err == nil {
		t.Fatal("Send() = nil, want the quota error surfaced")
	}
	if _, paths := f.recorded(); len(paths) != 1 {
		t.Fatalf("attempts = %d, want exactly one: quota exhaustion must not be retried as active", len(paths))
	}
}

func TestSendRejectsUnknownChatFormat(t *testing.T) {
	_, a := newFakeREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "bare-openid", Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("Send() error = %v, want it to refuse a chat id with no scenario prefix", err)
	}
}

// The attachment paths are covered in media_send_test.go: this channel uploads bytes and sends
// msg_type=7 rather than announcing that it cannot.

func TestHandleDispatchPublishesInbound(t *testing.T) {
	tests := []struct {
		name       string
		eventType  string
		payload    string
		ic         InstanceConfig
		wantSender string
		wantChat   string
		wantKind   string
		wantPub    bool
	}{
		{
			name:       "c2c direct",
			eventType:  "C2C_MESSAGE_CREATE",
			payload:    `{"id":"MSG1","content":"你好","author":{"user_openid":"U1","username":"amy"}}`,
			ic:         InstanceConfig{DMPolicy: "open"},
			wantSender: "c2c:U1", wantChat: "c2c:U1", wantKind: "direct", wantPub: true,
		},
		{
			// QQ strips the @机器人 prefix from content on this event, so what reaches the
			// pipeline is the question itself.
			name:       "group at message",
			eventType:  "GROUP_AT_MESSAGE_CREATE",
			payload:    `{"id":"MSG2","content":" 帮我看看 ","group_openid":"G1","author":{"member_openid":"M1"}}`,
			ic:         InstanceConfig{GroupPolicy: "open"},
			wantSender: "member:M1", wantChat: "grp:G1", wantKind: "group", wantPub: true,
		},
		{
			// Full-message mode is the console firehose. With require_mention at its default
			// the bot stays out of unaddressed chatter.
			name:      "full group mode without any mention is ignored",
			eventType: "GROUP_MESSAGE_CREATE",
			payload:   `{"id":"MSG3","content":"闲聊","group_openid":"G1","author":{"member_openid":"M2"}}`,
			ic:        InstanceConfig{GroupPolicy: "open"},
			wantPub:   false,
		},
		{
			name:       "full group mode with a mention is handled",
			eventType:  "GROUP_MESSAGE_CREATE",
			payload:    `{"id":"MSG4","content":"这个怎么看","group_openid":"G1","author":{"member_openid":"M2"},"mentions":[{"member_openid":"BOT"}]}`,
			ic:         InstanceConfig{GroupPolicy: "open"},
			wantSender: "member:M2", wantChat: "grp:G1", wantKind: "group", wantPub: true,
		},
		{
			name:       "require_mention off answers every group message",
			eventType:  "GROUP_MESSAGE_CREATE",
			payload:    `{"id":"MSG5","content":"闲聊","group_openid":"G1","author":{"member_openid":"M2"}}`,
			ic:         InstanceConfig{GroupPolicy: "open", RequireMention: boolPtr(false)},
			wantSender: "member:M2", wantChat: "grp:G1", wantKind: "group", wantPub: true,
		},
		{
			name:      "dm policy disabled drops",
			eventType: "C2C_MESSAGE_CREATE",
			payload:   `{"id":"MSG6","content":"hi","author":{"user_openid":"U2"}}`,
			ic:        InstanceConfig{DMPolicy: "disabled"},
			wantPub:   false,
		},
		{
			// Group policy is checked before the mention gate, so a disabled group stays
			// silent even for an @ of the bot.
			name:      "group policy disabled drops an at message",
			eventType: "GROUP_AT_MESSAGE_CREATE",
			payload:   `{"id":"MSG7","content":"hi","group_openid":"G2","author":{"member_openid":"M3"}}`,
			ic:        InstanceConfig{GroupPolicy: "disabled"},
			wantPub:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bus.New()
			c, err := New(Credentials{AppID: "a", AppSecret: "b"}, tt.ic, b, nil)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			c.handleDispatch(tt.eventType, json.RawMessage(tt.payload))

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			msg, ok := b.ConsumeInbound(ctx)
			if !tt.wantPub {
				if ok {
					t.Fatalf("inbound published = %+v, want it dropped", msg)
				}
				return
			}
			if !ok {
				t.Fatal("no inbound message published, want one")
			}
			if msg.SenderID != tt.wantSender || msg.ChatID != tt.wantChat || msg.PeerKind != tt.wantKind {
				t.Fatalf("inbound = (%q,%q,%q), want (%q,%q,%q)",
					msg.SenderID, msg.ChatID, msg.PeerKind, tt.wantSender, tt.wantChat, tt.wantKind)
			}
			if msg.Metadata["message_id"] == "" {
				t.Fatal("metadata has no message_id, so a reply cannot reference the inbound")
			}
			if msg.TenantID != c.TenantID() {
				t.Fatalf("tenant = %s, want the channel instance tenant %s", msg.TenantID, c.TenantID())
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestHandleDispatchDropsUnattributableAndUnknownEvents(t *testing.T) {
	b := bus.New()
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{DMPolicy: "open"}, b, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// A group event with no member_openid must not be attributed to author.id.
	c.handleDispatch("GROUP_AT_MESSAGE_CREATE",
		json.RawMessage(`{"id":"M1","content":"x","group_openid":"G1","author":{"id":"weird"}}`))
	// An event type outside the subscribed intents is ignored without decoding.
	c.handleDispatch("INTERACTION_CREATE", json.RawMessage(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if msg, ok := b.ConsumeInbound(ctx); ok {
		t.Fatalf("inbound published = %+v, want both events dropped", msg)
	}
}

func TestHandleDispatchDeduplicatesRedelivery(t *testing.T) {
	// QQ documents that the same msg_id can be pushed twice; publishing both would run the
	// agent twice and answer the user twice.
	b := bus.New()
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{DMPolicy: "open"}, b, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	payload := json.RawMessage(`{"id":"DUP1","content":"一次就好","author":{"user_openid":"U1"}}`)

	c.handleDispatch("C2C_MESSAGE_CREATE", payload)
	c.handleDispatch("C2C_MESSAGE_CREATE", payload)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := b.ConsumeInbound(ctx); !ok {
		t.Fatal("first delivery missing")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if msg, ok := b.ConsumeInbound(ctx2); ok {
		t.Fatalf("second delivery published = %+v, want it deduplicated", msg)
	}
}

func TestChannelSatisfiesInterface(t *testing.T) {
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// The interface assertions live in qq.go; what matters here is that the compile-time
	// contract holds and the default naming is what the loader expects.
	var ch channels.Channel = c

	if ch.Name() != "qq" {
		t.Fatalf("Name() = %q, want the default instance name qq", ch.Name())
	}
	if ch.Type() != channels.TypeQQ {
		t.Fatalf("Type() = %q, want %q", ch.Type(), channels.TypeQQ)
	}
}
