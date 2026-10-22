package weixin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

func TestRenderText(t *testing.T) {
	tests := []struct {
		name  string
		items []messageItem
		want  string
	}{
		{"text only", []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "你好"}}}, "你好"},
		{
			// Voice arrives transcribed, so the ASR text is used instead of a placeholder.
			name:  "voice with server ASR",
			items: []messageItem{{ItemType: itemTypeVoice, VoiceItem: &voiceItem{Text: "帮我看下"}}},
			want:  "帮我看下",
		},
		{
			name:  "voice without ASR is a placeholder",
			items: []messageItem{{ItemType: itemTypeVoice, VoiceItem: &voiceItem{}}},
			want:  "[语音]",
		},
		{
			name:  "image and file are announced, not dropped",
			items: []messageItem{{ItemType: itemTypeImage, ImageItem: &imageItem{}}, {ItemType: itemTypeFile, FileItem: &fileItem{FileName: "a.pdf"}}},
			want:  "[图片]\n[文件: a.pdf]",
		},
		{
			name:  "text plus media keeps both",
			items: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "看图"}}, {ItemType: itemTypeImage}},
			want:  "看图\n[图片]",
		},
		{name: "empty item list", items: nil, want: ""},
		{name: "empty text item", items: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{}}}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderText(tt.items); got != tt.want {
				t.Fatalf("renderText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"empty falls back to the official host", "", defaultBaseURL, false},
		{"bare host gets https and a trailing slash", "redirect.weixin.qq.com", "https://redirect.weixin.qq.com/", false},
		{"weixin host accepted", "https://ilinkai.weixin.qq.com", "https://ilinkai.weixin.qq.com/", false},
		{"non-weixin host refused", "https://evil.example.com/", "", true},
		{"weixin lookalike suffix refused", "https://evilweixin.qq.com/", "", true},
		{"plain http refused", "http://ilinkai.weixin.qq.com/", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sanitizeBaseURL(%q) = %q, want rejection", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeBaseURL(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("sanitizeBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := newClient("ftp://ilinkai.weixin.qq.com", "t", ""); err == nil {
		t.Fatal("newClient accepted a non-http base URL")
	}
	if _, err := newClient(defaultBaseURL, "t", "socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("newClient accepted a non-http proxy")
	}
	c, err := newClient(defaultBaseURL, "t", "")
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	if !strings.HasSuffix(c.baseURL, "/") {
		t.Fatalf("baseURL = %q, want a trailing slash so path joins stay under it", c.baseURL)
	}
}

func TestAccountKeySeparatesInstances(t *testing.T) {
	a := accountKey("https://x.weixin.qq.com/", "token-a")
	b := accountKey("https://x.weixin.qq.com/", "token-b")
	if a == b {
		t.Fatal("two tokens produced the same account key")
	}
	if len(a) != 16 {
		t.Fatalf("key = %q, want 16 hex characters", a)
	}
	if accountKey("", "") != "default" {
		t.Fatal("empty credentials should collapse to the default key")
	}
}

func TestStateCursorAndTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newState(dir, "https://x.weixin.qq.com/", "tok")

	if err := s.saveCursor("CURSOR-42"); err != nil {
		t.Fatalf("saveCursor() error = %v", err)
	}
	if err := s.rememberToken("user1", "CTX-1"); err != nil {
		t.Fatalf("rememberToken() error = %v", err)
	}

	restored := newState(dir, "https://x.weixin.qq.com/", "tok")
	if err := restored.load(); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got := restored.currentCursor(); got != "CURSOR-42" {
		t.Fatalf("cursor after reload = %q, want CURSOR-42", got)
	}
	tok, err := restored.replyToken("user1")
	if err != nil || tok != "CTX-1" {
		t.Fatalf("replyToken(user1) = (%q,%v), want CTX-1", tok, err)
	}

	// A different credential pair must not see this state.
	other := newState(dir, "https://x.weixin.qq.com/", "other-token")
	if err := other.load(); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if other.currentCursor() != "" {
		t.Fatalf("other instance read cursor %q, want isolation", other.currentCursor())
	}
}

func TestStateReplyTokenExpiry(t *testing.T) {
	s := newState(t.TempDir(), "https://x.weixin.qq.com/", "tok")
	if err := s.rememberToken("user1", "CTX-1"); err != nil {
		t.Fatalf("rememberToken() error = %v", err)
	}
	s.mu.Lock()
	s.tokens["user1"] = time.Now().Add(-contextTokenTTL - time.Hour)
	s.mu.Unlock()

	if _, err := s.replyToken("user1"); err == nil {
		t.Fatal("replyToken() accepted an expired context token")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want it to say the token expired", err)
	}
	if _, err := s.replyToken("never-messaged"); err == nil || !strings.Contains(err.Error(), "messaged it first") {
		t.Fatalf("error = %v, want the no-reply-context explanation", err)
	}
}

func TestStatePauseSession(t *testing.T) {
	s := newState(t.TempDir(), "https://x.weixin.qq.com/", "tok")
	if _, _, paused := s.paused(); paused {
		t.Fatal("fresh state should not be paused")
	}
	s.pauseSession("session expired", time.Hour)
	left, reason, paused := s.paused()
	if !paused || left <= 0 || reason == "" {
		t.Fatalf("paused() = (%v,%q,%v), want an active pause with a reason", left, reason, paused)
	}
}

func TestStatePathStaysInDirectory(t *testing.T) {
	dir := t.TempDir()
	s := newState(dir, "", "")
	if err := s.saveCursor("x"); err != nil {
		t.Fatalf("saveCursor() error = %v", err)
	}
	p := s.path("sync")
	if !strings.HasPrefix(p, filepath.Clean(dir)) {
		t.Fatalf("state path %q escaped the directory %q", p, dir)
	}
}

// fakeILink serves the CGI endpoints on one host so the poll loop, headers and send body can
// be asserted the way picoclaw's own tests pin them.
type fakeILink struct {
	replies  []string // raw JSON bodies returned by getupdates, in order
	headers  http.Header
	sentReqs []sendMessageReq
	uploads  []getUploadURLReq
	// uploadReply is the getuploadurl body. Empty means "hand back a relative slot", which is
	// the shape the outbound tests need to see rebuilt against the pinned CDN host.
	uploadReply string
	next        int
}

func (f *fakeILink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.headers = r.Header.Clone()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/ilink/bot/getupdates":
		i := f.next
		if i >= len(f.replies) {
			i = len(f.replies) - 1
		}
		f.next++
		_, _ = w.Write([]byte(f.replies[i]))
	case "/ilink/bot/sendmessage":
		var req sendMessageReq
		_ = json.Unmarshal(raw, &req)
		f.sentReqs = append(f.sentReqs, req)
		_, _ = w.Write([]byte(`{"ret":0}`))
	case "/ilink/bot/getuploadurl":
		var req getUploadURLReq
		_ = json.Unmarshal(raw, &req)
		f.uploads = append(f.uploads, req)
		if f.uploadReply != "" {
			_, _ = w.Write([]byte(f.uploadReply))
			return
		}
		_, _ = w.Write([]byte(`{"ret":0,"upload_param":"up-slot-1"}`))
	default:
		http.Error(w, `{"ret":-1,"errmsg":"unexpected path"}`, http.StatusNotFound)
	}
}

func newTestChannel(t *testing.T, baseURL string, msgBus *bus.MessageBus) *Channel {
	t.Helper()
	c, err := New(Credentials{Token: "tok", IlinkUserID: "ilink-user"}, InstanceConfig{BaseURL: baseURL}, msgBus, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.st = newState(t.TempDir(), baseURL, "tok")
	return c
}

func TestSendCarriesContextTokenAndHeaders(t *testing.T) {
	fake := &fakeILink{replies: []string{`{"ret":0,"get_updates_buf":"c1"}`}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := newTestChannel(t, srv.URL, bus.New())
	c.SetRunning(true)
	if err := c.st.rememberToken("user1", "CTX-9"); err != nil {
		t.Fatalf("rememberToken() error = %v", err)
	}

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "user1", Content: "收到"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(fake.sentReqs) != 1 {
		t.Fatalf("sendmessage calls = %d, want 1", len(fake.sentReqs))
	}
	sent := fake.sentReqs[0].Msg
	if sent.ContextToken != "CTX-9" {
		t.Fatalf("context_token = %q, want the token stored for this peer", sent.ContextToken)
	}
	if sent.ToUserID != "user1" || sent.MessageType != 2 || sent.MessageState != 2 {
		t.Fatalf("sent envelope = %+v, want to_user_id/message_type=2/message_state=2", sent)
	}
	if sent.ClientID == "" || !strings.HasPrefix(sent.ClientID, "goclaw-") {
		t.Fatalf("client_id = %q, want a goclaw-prefixed unique id", sent.ClientID)
	}
	if len(sent.ItemList) != 1 || sent.ItemList[0].TextItem == nil || sent.ItemList[0].TextItem.Text != "收到" {
		t.Fatalf("item list = %+v, want one text item", sent.ItemList)
	}
	if got := fake.headers.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", got)
	}
	if got := fake.headers.Get("iLink-App-Id"); got != "bot" {
		t.Fatalf("iLink-App-Id = %q, want bot", got)
	}
	if got := fake.headers.Get("AuthorizationType"); got != "ilink_bot_token" {
		t.Fatalf("AuthorizationType = %q, want ilink_bot_token", got)
	}
	if fake.headers.Get("X-WECHAT-UIN") == "" {
		t.Fatal("X-WECHAT-UIN missing; iLink expects a value on every authenticated call")
	}
}

// The protocol cannot deliver an unsolicited message. Cron and heartbeat output must fail
// with that explanation rather than look like it was sent.
func TestSendRefusesWithoutReplyContext(t *testing.T) {
	fake := &fakeILink{replies: []string{`{"ret":0}`}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c := newTestChannel(t, srv.URL, bus.New())
	c.SetRunning(true)

	err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "stranger", Content: "定时提醒"})
	if err == nil {
		t.Fatal("Send() = nil, want the undeliverable case reported")
	}
	if !strings.Contains(err.Error(), "messaged it first") {
		t.Fatalf("error = %v, want it to explain the peer must message first", err)
	}
	if len(fake.sentReqs) != 0 {
		t.Fatalf("sendmessage calls = %d, want none", len(fake.sentReqs))
	}
}

func TestPollLoopAdvancesCursorAndHandlesSessionExpiry(t *testing.T) {
	batch := `{"ret":0,"msgs":[{"from_user_id":"u1","context_token":"CTX","item_list":[{"type":1,"text_item":{"text":"在吗"}}]}],"get_updates_buf":"cursor-2"}`
	fake := &fakeILink{replies: []string{batch, `{"ret":-14,"errmsg":"session expired"}`}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	msgBus := bus.New()
	// dm_policy=open: with the default "pairing" and no pairing store the message is
	// correctly refused, which is what TestHandleInboundDrops pins. This test wants the
	// loop itself, so the policy gate is opened.
	c := newTestChannel(t, srv.URL, msgBus)
	c.dmPolicy = "open"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.pollLoop(ctx)

	ctxRead, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRead()
	msg, ok := msgBus.ConsumeInbound(ctxRead)
	if !ok {
		t.Fatal("no inbound published, want the polled message delivered")
	}
	if msg.SenderID != "u1" || msg.ChatID != "u1" || msg.Content != "在吗" || msg.PeerKind != "direct" {
		t.Fatalf("inbound = %+v, want u1/在吗/direct", msg)
	}
	if msg.Metadata["message_id"] == "" {
		t.Log("message_id metadata absent; acceptable when the platform sent no client_id")
	}

	// The reply reference must survive into state, and the cursor must be persisted.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tok, err := c.st.replyToken("u1"); err == nil && tok == "CTX" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tok, err := c.st.replyToken("u1"); err != nil || tok != "CTX" {
		t.Fatalf("replyToken(u1) = (%q,%v), want the token from the inbound", tok, err)
	}
	if got := c.st.currentCursor(); got != "cursor-2" {
		t.Fatalf("cursor = %q, want cursor-2 after the batch was handed over", got)
	}

	// The second reply carries -14, which must park the session rather than spin.
	for time.Now().Before(deadline) {
		if _, reason, paused := c.st.paused(); paused {
			if !strings.Contains(reason, "-14") {
				t.Fatalf("pause reason = %q, want it to name the -14 session expiry", reason)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session was not paused after a -14 response")
}

func TestHandleInboundDrops(t *testing.T) {
	tests := []struct {
		name string
		ic   InstanceConfig
		msg  WeixinMessage
	}{
		{
			name: "empty content", ic: InstanceConfig{DMPolicy: "open"},
			msg: WeixinMessage{FromUserID: "u1", ItemList: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{}}}},
		},
		{
			name: "no sender", ic: InstanceConfig{DMPolicy: "open"},
			msg: WeixinMessage{ItemList: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "hi"}}}},
		},
		{
			// An iLink bot cannot join groups and the reply reference is per peer, so a
			// group message has no defined answer path.
			name: "group message", ic: InstanceConfig{DMPolicy: "open"},
			msg: WeixinMessage{FromUserID: "u1", GroupID: "g1", ItemList: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "hi"}}}},
		},
		{
			// The default policy is pairing, and without a pairing store there is nothing to
			// pair against: the message must be refused, not waved through.
			name: "pairing without a pairing store fails closed", ic: InstanceConfig{},
			msg: WeixinMessage{FromUserID: "u1", ContextToken: "CTX", ItemList: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "hi"}}}},
		},
		{
			name: "dm policy disabled", ic: InstanceConfig{DMPolicy: "disabled"},
			msg: WeixinMessage{FromUserID: "u1", ItemList: []messageItem{{ItemType: itemTypeText, TextItem: &textItem{Text: "hi"}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bus.New()
			c, err := New(Credentials{Token: "tok"}, tt.ic, b, nil)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			c.st = newState(t.TempDir(), defaultBaseURL, "tok")

			c.handleInbound(tt.msg)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if msg, ok := b.ConsumeInbound(ctx); ok {
				t.Fatalf("inbound published = %+v, want it dropped", msg)
			}
		})
	}
}

func TestFactoryMissingToken(t *testing.T) {
	ch, err := Factory("wx", json.RawMessage(`{"token":""}`), nil, nil, nil)
	if err != nil {
		t.Fatalf("Factory() error = %v, want nil for an unscanned instance", err)
	}
	if ch != nil {
		t.Fatalf("Factory() = %v, want nil so the loader reports missing activation", ch)
	}
	if _, err := Factory("wx", json.RawMessage(`{oops`), nil, nil, nil); err == nil {
		t.Fatal("Factory() = nil error, want a decode failure reported")
	}
	ch, err = Factory("wx-main", json.RawMessage(`{"token":"t","ilink_user_id":"iu"}`), json.RawMessage(`{"dm_policy":"open"}`), nil, nil)
	if err != nil {
		t.Fatalf("Factory() error = %v", err)
	}
	if ch == nil {
		t.Fatal("Factory() = nil channel, want a configured one")
	}
	if ch.Name() != "wx-main" {
		t.Fatalf("Name() = %q, want the instance name", ch.Name())
	}
}

// Start must refuse to pretend it is working without a token: an unscanned instance should
// surface as a config failure in the health detail.
func TestStartWithoutToken(t *testing.T) {
	c, err := New(Credentials{}, InstanceConfig{}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start() = nil, want ErrNotActivated")
	}
}
