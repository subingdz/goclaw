package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

func newTestChannel(t *testing.T) *Channel {
	t.Helper()
	c, err := New(Credentials{AppID: "app", AppSecret: "secret"}, InstanceConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func TestParseExpiresIn(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"number", `7200`, 7200, false},
		// The current docs print both shapes for the same field, so both must work.
		{"quoted number", `"7200"`, 7200, false},
		{"missing", ``, 0, true},
		{"null", `null`, 0, true},
		{"not a number", `"abc"`, 0, true},
		{"zero", `0`, 0, true},
		{"negative", `-5`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExpiresIn(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseExpiresIn(%s) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseExpiresIn(%s) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSplitChatID(t *testing.T) {
	tests := []struct {
		chatID      string
		wantKind    string
		wantTarget  string
		wantErrPart string
	}{
		{chatPrefixC2C + "USER123", "c2c", "USER123", ""},
		{chatPrefixGroup + "GRP456", "group", "GRP456", ""},
		{"USER123", "", "", "missing the"},
	}
	for _, tt := range tests {
		t.Run(tt.chatID, func(t *testing.T) {
			kind, target, err := splitChatID(tt.chatID)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("splitChatID(%q) error = %v, want it to mention %q", tt.chatID, err, tt.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitChatID(%q) error = %v", tt.chatID, err)
			}
			if kind != tt.wantKind || target != tt.wantTarget {
				t.Fatalf("splitChatID(%q) = (%q,%q), want (%q,%q)", tt.chatID, kind, target, tt.wantKind, tt.wantTarget)
			}
		})
	}
}

func TestSenderKeyAndChatKey(t *testing.T) {
	tests := []struct {
		name       string
		evt        messageEvent
		wantSender string
		wantChat   string
		wantErr    bool
	}{
		{
			name:       "c2c uses user_openid",
			evt:        messageEvent{Author: messageAuthor{UserOpenID: "U1"}},
			wantSender: "c2c:U1",
			wantChat:   "c2c:U1",
		},
		{
			name:       "c2c falls back to author.id, same namespace",
			evt:        messageEvent{Author: messageAuthor{ID: "U2"}},
			wantSender: "c2c:U2",
			wantChat:   "c2c:U2",
		},
		{
			// A group message must be attributed to the member, never to author.id: the two
			// are different namespaces and merging them would carry one person's pairing into
			// another's identity.
			name:       "group uses member_openid",
			evt:        messageEvent{GroupOpenID: "G1", Author: messageAuthor{ID: "whatever", MemberOpenID: "M1"}},
			wantSender: "member:M1",
			wantChat:   "grp:G1",
		},
		{
			name:    "group without member_openid is dropped",
			evt:     messageEvent{GroupOpenID: "G1", Author: messageAuthor{ID: "whatever"}},
			wantErr: true,
		},
		{
			name:    "c2c without any id is dropped",
			evt:     messageEvent{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := tt.evt
			sender, err := evt.senderKey()
			if tt.wantErr {
				if err == nil {
					t.Fatal("senderKey() = nil error, want rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("senderKey() error = %v", err)
			}
			if sender != tt.wantSender {
				t.Fatalf("senderKey() = %q, want %q", sender, tt.wantSender)
			}
			chat, err := evt.chatKey()
			if err != nil {
				t.Fatalf("chatKey() error = %v", err)
			}
			if chat != tt.wantChat {
				t.Fatalf("chatKey() = %q, want %q", chat, tt.wantChat)
			}
		})
	}
}

func TestIsPassiveFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"window or budget exceeded", &apiError{ErrCode: errPassiveWindow}, true},
		{"msg_id expired", &apiError{ErrCode: errPassiveExpired}, true},
		{"legacy id expired", &apiError{ErrCode: errPassiveIDExpired}, true},
		{"invalid or unauthorized msg_id", &apiError{ErrCode: errPassiveIDInvalid}, true},
		{
			// Active-message quota is a different problem: falling back to an active message
			// is exactly what already failed, so it must not be retried that way.
			name: "active quota exceeded", err: &apiError{ErrCode: errPassiveQuotaExceed}, want: false,
		},
		{"legacy code field", &apiError{Code: errPassiveExpired}, true},
		{"plain error", fmt.Errorf("network"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPassiveFailure(tt.err); got != tt.want {
				t.Fatalf("isPassiveFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestApiErrorJudgesByErrCodeNotMessage(t *testing.T) {
	// The docs forbid deciding success from "message"; only err_code (or the legacy code)
	// may decide, so a non-zero message with err_code 0 must not look like a failure.
	var zero apiError
	if err := json.Unmarshal([]byte(`{"err_code":0,"message":"随便写的文案"}`), &zero); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if zero.code() != 0 {
		t.Fatalf("code() = %d, want 0 regardless of message text", zero.code())
	}
}

func TestPassiveBudgetAndWindow(t *testing.T) {
	c := newTestChannel(t)
	chat := chatPrefixC2C + "U1"
	c.noteInbound(chat, "MSG1")

	if p := c.passiveRef(chat); p == nil || p.msgID != "MSG1" {
		t.Fatalf("passiveRef() = %+v, want the just-received msg id", p)
	}
	// The reply budget is per msg_id and must be enforced: past it, QQ rejects the send.
	for i := 0; i < passiveReplyBudget; i++ {
		c.consumePassive(chat, true)
	}
	if p := c.passiveRef(chat); p != nil {
		t.Fatalf("passiveRef() after %d replies = %+v, want the budget spent", passiveReplyBudget, p)
	}

	c.noteInbound(chat, "MSG2")
	c.mu.Lock()
	c.passives[chat].started = time.Now().Add(-passiveReplyWindow * 2 * time.Second)
	c.mu.Unlock()
	if p := c.passiveRef(chat); p != nil {
		t.Fatalf("passiveRef() past the window = %+v, want expiry", p)
	}

	c.noteInbound(chat, "MSG3")
	c.forgetPassive(chat)
	if p := c.passiveRef(chat); p != nil {
		t.Fatalf("passiveRef() after forget = %+v, want nil", p)
	}
}

func TestNewSendRequestSeq(t *testing.T) {
	c := newTestChannel(t)
	chat := chatPrefixGroup + "G1"

	if req := c.newSendRequest(chat, "a", 0); req.MsgID != "" || req.MsgSeq != 1 {
		t.Fatalf("first chunk with no inbound reference = %+v, want active msg with seq 1", req)
	}
	// Later chunks of one answer cannot reuse the passive reference; QQ counts them.
	if req := c.newSendRequest(chat, "b", 2); req.MsgID != "" || req.MsgSeq != 3 {
		t.Fatalf("third chunk = %+v, want active msg with seq 3", req)
	}

	c.noteInbound(chat, "MID")
	if req := c.newSendRequest(chat, "a", 0); req.MsgID != "MID" || req.MsgSeq != 1 {
		t.Fatalf("first chunk after inbound = %+v, want msg_id=MID seq=1", req)
	}
	c.consumePassive(chat, true)
	if req := c.newSendRequest(chat, "a", 0); req.MsgSeq != 2 {
		t.Fatalf("second passive reply seq = %d, want 2", req.MsgSeq)
	}
}

func TestDuplicateEventDroppedAndRingBounded(t *testing.T) {
	c := newTestChannel(t)
	if c.duplicate("E1") {
		t.Fatal("duplicate(E1) = true on first sight, want false")
	}
	if !c.duplicate("E1") {
		t.Fatal("duplicate(E1) = false on redelivery, want true (QQ documents at-least-once)")
	}
	// A live gateway must not grow this map without bound.
	for i := 0; i < duplicateRetention*3; i++ {
		c.duplicate(fmt.Sprintf("E%d", i))
	}
	c.mu.Lock()
	size := len(c.seenEvent)
	c.mu.Unlock()
	if size > duplicateRetention {
		t.Fatalf("seenEvent size = %d, want it capped at %d", size, duplicateRetention)
	}
}

func TestCloseCodeVerdict(t *testing.T) {
	permanent := []int{4914, 4915, 4013, 4014, 4010, 4011}
	for _, code := range permanent {
		advice, isPerm := closeCodeVerdict(code)
		if !isPerm || advice == "" {
			t.Fatalf("closeCodeVerdict(%d) = (%q,%v), want permanent with advice", code, advice, isPerm)
		}
	}
	for _, code := range []int{4001, 4002, 4006, 4007, 4008, 4009, 4012, 4900} {
		if _, isPerm := closeCodeVerdict(code); isPerm {
			t.Fatalf("closeCodeVerdict(%d) marked permanent, but the docs say reconnect", code)
		}
	}
}

func TestNewAPIValidation(t *testing.T) {
	tests := []struct {
		name      string
		base      string
		appID     string
		secret    string
		wantErr   bool
		errSubstr string
	}{
		{"defaults applied", "", "a", "b", false, ""},
		{"missing app id", "", "", "b", true, "app_id"},
		{"missing secret", "", "a", "", true, "app_secret"},
		{"non-http scheme rejected", "ftp://example.com", "a", "b", true, "absolute http(s)"},
		{"hostless url rejected", "http://", "a", "b", true, "absolute http(s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newAPI(tt.base, tt.appID, tt.secret)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("newAPI() error = %v, want it to mention %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newAPI() error = %v", err)
			}
			if a.base != defaultAPIBase {
				t.Fatalf("base = %q, want the documented unified host %q", a.base, defaultAPIBase)
			}
		})
	}
}

// TestAccessTokenCachesUntilSkew covers the documented renewal rule: one token per app, no
// refresh token, and a request only yields a new token inside the final minute.
func TestAccessTokenCachesUntilSkew(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, tokenPath)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// The field names are appId/clientSecret, not the OAuth2 client_id/secret pair.
		if body["appId"] != "app" || body["clientSecret"] != "secret" {
			t.Fatalf("token body = %+v, want appId/clientSecret", body)
		}
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok%d", calls), "expires_in": 7200})
	}))
	t.Cleanup(srv.Close)

	a, err := newAPI(srv.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	ctx := context.Background()

	first, err := a.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() error = %v", err)
	}
	second, err := a.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() error = %v", err)
	}
	if first != second || calls != 1 {
		t.Fatalf("tokens = %q/%q after %d fetches, want one cached fetch", first, second, calls)
	}

	a.mu.Lock()
	a.renewAt = time.Now().Add(-time.Second)
	a.mu.Unlock()
	third, err := a.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken() after expiry error = %v", err)
	}
	if third == first || calls != 2 {
		t.Fatalf("token = %q after %d fetches, want a fresh one", third, calls)
	}
}

func TestCallJudgesErrCodeAndSendsHeader(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		switch r.URL.Path {
		case tokenPath:
			// expires_in is printed both unquoted and quoted in the current docs; this
			// fixture sends the quoted form.
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": "7200"})
		case "/v2/groups/G1/messages":
			// A 200 with a non-zero err_code is a failure — this shape is why success must
			// never be judged by HTTP status alone.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"err_code":40034128,"message":"被动回复时间或次数超限","trace_id":"t1"}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	a, err := newAPI(srv.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	err = a.send(context.Background(), "group", "G1", sendRequest{MsgType: msgTypeText, Content: "hi"})
	if err == nil {
		t.Fatal("send() = nil, want the err_code surfaced as a failure")
	}
	if !isPassiveFailure(err) {
		t.Fatalf("send() error = %v, want it classified as a passive-reply failure", err)
	}
	if authHeader != "QQBot tok" {
		t.Fatalf("Authorization = %q, want the documented QQBot {token} form", authHeader)
	}
}

func TestFactoryMissingCredentials(t *testing.T) {
	ch, err := Factory("qq", json.RawMessage(`{"app_id":"","app_secret":""}`), nil, nil, nil)
	if err != nil {
		t.Fatalf("Factory() error = %v, want nil for an unconfigured instance", err)
	}
	if ch != nil {
		t.Fatalf("Factory() = %v, want nil channel so the loader reports missing credentials", ch)
	}

	if _, err := Factory("qq", json.RawMessage(`{oops`), nil, nil, nil); err == nil {
		t.Fatal("Factory() = nil error, want a decode failure reported")
	}

	ch, err = Factory("my-qq", json.RawMessage(`{"app_id":"a","app_secret":"b"}`),
		json.RawMessage(`{"dm_policy":"open","text_chunk_limit":300}`), nil, nil)
	if err != nil {
		t.Fatalf("Factory() error = %v", err)
	}
	if ch == nil {
		t.Fatal("Factory() = nil channel, want a configured one")
	}
	if got := ch.Name(); got != "my-qq" {
		t.Fatalf("Name() = %q, want the instance name", got)
	}
	if got := ch.Type(); got != channels.TypeQQ {
		t.Fatalf("Type() = %q, want %q", got, channels.TypeQQ)
	}
}
