package qq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeGateway serves both the REST endpoints and the websocket on one httptest server, so
// the session loop is exercised over a real socket without touching the internet.
type fakeGateway struct {
	t    *testing.T
	srv  *httptest.Server
	wsOK string

	mu         sync.Mutex
	conns      int
	identifys  []identifyPayload
	resumes    []resumePayload
	heartbeats []int64

	sig chan string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t, sig: make(chan string, 64)}

	// The default same-origin policy is used deliberately: the test client is a Go dialer
	// with no Origin header, which gorilla accepts, and a permissive CheckOrigin here would
	// be the kind of looseness that spreads into a real server.
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()

	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 7200})
		f.signal("token")
	})
	mux.HandleFunc(gatewayPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"url": f.wsOK})
		f.signal("gateway")
	})
	mux.HandleFunc("/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			f.t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		f.mu.Lock()
		f.conns++
		n := f.conns
		f.mu.Unlock()
		f.serveConn(conn, n)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.wsOK = "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/websocket"
	return f
}

func (f *fakeGateway) signal(name string) {
	select {
	case f.sig <- name:
	default:
	}
}

// awaitSignal waits generously: the reconnect path deliberately sleeps with backoff, and a
// tight timeout here would only make the suite flaky on a loaded machine. Signals are
// consumed in emission order, so draining past the ones not waited for is safe.
func (f *fakeGateway) awaitSignal(name string, timeout time.Duration) {
	f.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case got := <-f.sig:
			if got == name {
				return
			}
		case <-deadline.C:
			f.t.Fatalf("timed out waiting for %q", name)
		}
	}
}

func (f *fakeGateway) serveConn(conn *websocket.Conn, n int) {
	// A short heartbeat interval keeps the test honest about the loop without making it
	// slow: the client must still drive identify, ack-driven liveness and reconnect.
	f.writeFrame(conn, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 150}})

	switch n {
	case 1:
		f.firstConnection(conn)
	case 2:
		f.secondConnection(conn)
	default:
		f.t.Errorf("unexpected third websocket connection (%d)", n)
	}
}

func (f *fakeGateway) firstConnection(conn *websocket.Conn) {
	var fr frame
	if err := conn.ReadJSON(&fr); err != nil {
		f.t.Errorf("read identify: %v", err)
		return
	}
	if fr.Op != opIdentify {
		f.t.Errorf("first client frame op = %d, want identify (2)", fr.Op)
		return
	}
	var idp identifyPayload
	if err := json.Unmarshal(fr.D, &idp); err != nil {
		f.t.Errorf("decode identify: %v", err)
		return
	}
	f.mu.Lock()
	f.identifys = append(f.identifys, idp)
	f.mu.Unlock()
	f.signal("identify")

	f.writeFrame(conn, map[string]any{
		"op": opDispatch, "s": 1, "t": "READY",
		"d": map[string]any{"version": 1, "session_id": "S1", "user": map[string]any{"username": "bot"}},
	})
	f.signal("ready")

	f.writeFrame(conn, map[string]any{
		"op": opDispatch, "s": 2, "t": "C2C_MESSAGE_CREATE",
		"id": "EV1",
		"d": map[string]any{
			"id":      "MSG1",
			"content": "你好",
			"author":  map[string]any{"user_openid": "U1", "username": "tester"},
		},
	})

	// Two heartbeats are consumed so the dispatch has certainly been drained before the
	// close, without asserting which sequence number each one carried.
	for i := 0; i < 2; i++ {
		if err := conn.ReadJSON(&fr); err != nil {
			f.t.Errorf("read heartbeat: %v", err)
			return
		}
		if fr.Op != opHeartbeat {
			f.t.Errorf("frame op = %d, want heartbeat (1)", fr.Op)
			return
		}
		var seq int64
		_ = json.Unmarshal(fr.D, &seq)
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, seq)
		f.mu.Unlock()
		f.writeFrame(conn, map[string]any{"op": opHeartbeatACK})
	}
	f.signal("heartbeat")

	// 连接过期，请重连并执行 resume: the session must survive the drop.
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(4009, "expired"), time.Now().Add(time.Second))
}

func (f *fakeGateway) secondConnection(conn *websocket.Conn) {
	var fr frame
	if err := conn.ReadJSON(&fr); err != nil {
		f.t.Errorf("read resume: %v", err)
		return
	}
	if fr.Op != opResume {
		f.t.Errorf("second connection op = %d, want resume (6)", fr.Op)
		return
	}
	var rp resumePayload
	if err := json.Unmarshal(fr.D, &rp); err != nil {
		f.t.Errorf("decode resume: %v", err)
		return
	}
	f.mu.Lock()
	f.resumes = append(f.resumes, rp)
	f.mu.Unlock()
	f.signal("resume")

	f.writeFrame(conn, map[string]any{"op": opDispatch, "s": 3, "t": "RESUMED", "d": map[string]any{}})
	// Keep the socket open until the test cancels the context.
	for {
		var idle frame
		if err := conn.ReadJSON(&idle); err != nil {
			return
		}
		if idle.Op == opHeartbeat {
			f.writeFrame(conn, map[string]any{"op": opHeartbeatACK})
		}
	}
}

func (f *fakeGateway) writeFrame(conn *websocket.Conn, v any) {
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteJSON(v); err != nil {
		f.t.Errorf("server write: %v", err)
	}
}

func (f *fakeGateway) api(t *testing.T) *api {
	t.Helper()
	a, err := newAPI(f.srv.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	return a
}

func TestGatewayIdentifyHeartbeatAndResume(t *testing.T) {
	f := newFakeGateway(t)

	var mu sync.Mutex
	gotEvents := map[string]int{}

	g := &gateway{
		api:     f.api(t),
		intents: intentGroupAndC2C,
		onDispatch: func(evtType string, d json.RawMessage) {
			mu.Lock()
			gotEvents[evtType]++
			mu.Unlock()
			f.signal("dispatch")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- g.serve(ctx) }()

	f.awaitSignal("identify", 15*time.Second)
	f.awaitSignal("dispatch", 15*time.Second)
	f.awaitSignal("heartbeat", 15*time.Second)
	// The 4009 close sends us through the backoff sleep before the second dial.
	f.awaitSignal("resume", 30*time.Second)

	f.mu.Lock()
	identifys := append([]identifyPayload(nil), f.identifys...)
	resumes := append([]resumePayload(nil), f.resumes...)
	heartbeats := append([]int64(nil), f.heartbeats...)
	f.mu.Unlock()

	mu.Lock()
	events := make(map[string]int, len(gotEvents))
	for k, v := range gotEvents {
		events[k] = v
	}
	mu.Unlock()

	if len(identifys) != 1 {
		t.Fatalf("identify frames = %d, want 1", len(identifys))
	}
	// The docs put the token inside the socket, in the same QQBot form the REST header uses.
	if identifys[0].Token != "QQBot tok" {
		t.Fatalf("identify token = %q, want %q", identifys[0].Token, "QQBot tok")
	}
	if identifys[0].Intents != intentGroupAndC2C {
		t.Fatalf("identify intents = %d, want GROUP_AND_C2C_EVENT (%d)", identifys[0].Intents, int64(intentGroupAndC2C))
	}
	if identifys[0].Shard != [2]int{0, 1} {
		t.Fatalf("identify shard = %v, want [0 1] for a single-shard bot", identifys[0].Shard)
	}
	if events["C2C_MESSAGE_CREATE"] != 1 {
		t.Fatalf("dispatched events = %+v, want one C2C_MESSAGE_CREATE", events)
	}
	if len(heartbeats) != 2 {
		t.Fatalf("heartbeats = %v, want two", heartbeats)
	}
	for _, seq := range heartbeats {
		if seq < 1 {
			t.Fatalf("heartbeat carried seq %d, want the last received dispatch sequence", seq)
		}
	}
	if len(resumes) != 1 {
		t.Fatalf("resume frames = %d, want 1", len(resumes))
	}
	if resumes[0].SessionID != "S1" {
		t.Fatalf("resume session_id = %q, want the READY session id S1", resumes[0].SessionID)
	}
	if resumes[0].Seq < 1 {
		t.Fatalf("resume seq = %d, want the sequence reached before the drop", resumes[0].Seq)
	}
	if resumes[0].Token != "QQBot tok" {
		t.Fatalf("resume token = %q, want QQBot tok", resumes[0].Token)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil && err != context.Canceled {
			t.Fatalf("serve() after cancel = %v, want nil or context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after cancel")
	}
}

// TestGatewayStopsOnConsoleSideFailure pins the one behaviour that must not loop: when the
// gateway says the intents are not approved, retrying is noise, and the operator needs the
// advice in the health detail.
func TestGatewayStopsOnConsoleSideFailure(t *testing.T) {
	var wsURL string
	mux2 := http.NewServeMux()
	upgrader := websocket.Upgrader{} // default same-origin policy; the test client sends no Origin

	srv2 := httptest.NewServer(mux2)
	wsURL = "ws" + strings.TrimPrefix(srv2.URL, "http") + "/ws"
	t.Cleanup(srv2.Close)

	mux2.HandleFunc(gatewayPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"url": wsURL})
	})
	mux2.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 7200})
	})
	mux2.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 200}})
		var fr frame
		_ = conn.ReadJSON(&fr)
		// 4014 = intent 无权限: nothing the program can fix.
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(4014, "intent not approved"), time.Now().Add(time.Second))
	})

	a, err := newAPI(srv2.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	g := &gateway{api: a, intents: intentGroupAndC2C, onDispatch: func(string, json.RawMessage) {}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = g.serve(ctx)
	var perm *permanentError
	if !errors.As(err, &perm) {
		t.Fatalf("serve() error = %v, want a permanentError for close code 4014", err)
	}
	if !strings.Contains(err.Error(), "intent") {
		t.Fatalf("serve() error = %q, want it to carry the console advice", err)
	}
}
