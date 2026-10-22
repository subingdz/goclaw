package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errReconnect is returned internally when the gateway asked us to reconnect (op 7) or the
// session was invalidated. It is not a channel failure: the serve loop reconnects and
// resumes.
var errReconnect = errors.New("qq: reconnect requested")

// permanentError wraps a cause the gateway will keep refusing, so the reconnect loop stops
// instead of hammering an endpoint that needs a change in the developer console.
type permanentError struct{ reason error }

func (e *permanentError) Error() string { return e.reason.Error() }

// gateway owns the websocket session and the reconnect loop above it. It keeps exactly the
// two pieces of state the protocol requires across connections — session_id and the last
// dispatch sequence — because op 6 Resume is rejected unless both are carried forward.
type gateway struct {
	api     *api
	intents int64
	// wsURL is the endpoint /gateway handed us. It is reused across reconnects because
	// re-querying on every drop turns one flap into two requests per retry.
	wsURL string
	// onDispatch receives every op-0 frame except READY and RESUMED, which this loop
	// consumes itself.
	onDispatch func(t string, d json.RawMessage)

	mu        sync.Mutex
	sessionID string
	seq       int64
	resumable bool
	lastAck   time.Time
	hb        time.Duration

	connSeq uint64
}

// serve runs connect/read/reconnect until ctx is cancelled or the gateway refuses us.
// Backoff lives here, in one place, so what "this channel is unhealthy" means has a single
// definition.
func (g *gateway) serve(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := g.runOnce(ctx)
		var perm *permanentError
		if errors.As(err, &perm) {
			return perm
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, errReconnect) {
			slog.Warn("qq: gateway session ended", "error", err)
		}

		g.mu.Lock()
		resumed := g.resumable
		g.mu.Unlock()
		if resumed {
			// Getting as far as READY means the endpoint was willing to talk to us, so the
			// next attempt starts from the short wait again rather than the accumulated one.
			backoff = 2 * time.Second
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// runOnce opens one connection and processes frames until it ends.
func (g *gateway) runOnce(ctx context.Context) error {
	wsURL := g.wsURL
	if wsURL == "" {
		var err error
		if wsURL, err = g.api.gatewayURL(ctx); err != nil {
			return fmt.Errorf("qq: gateway url: %w", err)
		}
		g.wsURL = wsURL
	}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	// No Authorization header: the current docs authenticate the websocket inside the
	// socket, at Identify. (The header form belongs to the REST calls.)
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			// The token can expire between the REST lookup and the handshake. Drop it so the
			// next round authenticates fresh.
			g.api.invalidate()
		}
		return fmt.Errorf("qq: dial: %w", err)
	}
	defer conn.Close()

	g.mu.Lock()
	g.connSeq++
	id := g.connSeq
	g.mu.Unlock()

	slog.Info("qq: websocket connected", "conn", id)
	return g.pump(ctx, conn, id)
}

// pump performs hello → identify-or-resume and then serves frames.
func (g *gateway) pump(ctx context.Context, conn *websocket.Conn, id uint64) error {
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var hello frame
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("qq: read hello: %w", err)
	}
	if hello.Op != opHello {
		return &permanentError{fmt.Errorf("qq: first frame was op %d, expected hello (10)", hello.Op)}
	}

	var hp helloPayload
	hb := defaultHeartbeatMs * time.Millisecond
	if err := json.Unmarshal(hello.D, &hp); err == nil && hp.HeartbeatInterval > 0 {
		hb = time.Duration(hp.HeartbeatInterval) * time.Millisecond
	}
	g.mu.Lock()
	g.hb = hb
	g.lastAck = time.Now()
	g.mu.Unlock()

	token, err := g.api.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("qq: token for identify: %w", err)
	}

	g.mu.Lock()
	resume := g.resumable && g.sessionID != ""
	seq, sid := g.seq, g.sessionID
	g.mu.Unlock()

	if resume {
		if err := sendFrame(conn, opResume, mustJSON(resumePayload{
			Token: "QQBot " + token, SessionID: sid, Seq: seq,
		})); err != nil {
			return fmt.Errorf("qq: send resume: %w", err)
		}
		slog.Info("qq: resuming session", "conn", id, "seq", seq)
	} else {
		if err := sendFrame(conn, opIdentify, mustJSON(identifyPayload{
			Token:      "QQBot " + token,
			Intents:    g.intents,
			Shard:      [2]int{0, 1},
			Properties: map[string]any{"$os": "linux", "$browser": "goclaw", "$device": "goclaw"},
		})); err != nil {
			return fmt.Errorf("qq: send identify: %w", err)
		}
		slog.Info("qq: identifying", "conn", id, "intents", g.intents)
		g.mu.Lock()
		g.resumable, g.sessionID = false, ""
		g.mu.Unlock()
	}

	// The gorilla reader is not safe for concurrent use, so one goroutine owns all reads
	// and this loop owns all writes.
	readTimeout := 2*hb + 15*time.Second
	frames := make(chan frame, 32)
	readErr := make(chan error, 1)
	go func() { readErr <- readLoop(conn, frames, readTimeout) }()

	hbTick := time.NewTicker(hb)
	defer hbTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return g.verdict(err)
		case <-hbTick.C:
			g.mu.Lock()
			since, seq := time.Since(g.lastAck), g.seq
			g.mu.Unlock()
			// A full beat without an ACK means a zombie socket: the peer is gone but TCP has
			// not noticed, and queueing replies into it would look like delivery success.
			if since > 2*hb {
				return fmt.Errorf("qq: no heartbeat ack for %s", since)
			}
			if err := sendFrame(conn, opHeartbeat, json.RawMessage(strconv.FormatInt(seq, 10))); err != nil {
				return fmt.Errorf("qq: send heartbeat: %w", err)
			}
		case f := <-frames:
			if err := g.handleFrame(conn, id, f, hb); err != nil {
				return err
			}
		}
	}
}

// handleFrame applies one received frame. Sequence tracking happens for every downstream
// frame because Resume must report the last one the client saw.
func (g *gateway) handleFrame(conn *websocket.Conn, id uint64, f frame, hb time.Duration) error {
	if f.S != 0 {
		g.mu.Lock()
		g.seq = f.S
		g.mu.Unlock()
	}

	switch f.Op {
	case opDispatch:
		switch f.T {
		case "READY":
			var r readyPayload
			if err := json.Unmarshal(f.D, &r); err != nil {
				slog.Warn("qq: undecodable READY", "error", err)
				return nil
			}
			g.mu.Lock()
			g.sessionID, g.resumable, g.lastAck = r.SessionID, true, time.Now()
			g.mu.Unlock()
			slog.Info("qq: ready", "conn", id, "bot", r.User.Username, "session_id", r.SessionID)
			return nil
		case "RESUMED":
			g.mu.Lock()
			g.lastAck = time.Now()
			g.mu.Unlock()
			slog.Info("qq: session resumed", "conn", id)
			return nil
		default:
			g.onDispatch(f.T, f.D)
			return nil
		}

	case opHeartbeatACK:
		g.mu.Lock()
		g.lastAck = time.Now()
		g.mu.Unlock()
		return nil

	case opReconnect:
		// The gateway keeps the session alive across the reconnect, so resume is correct.
		g.mu.Lock()
		g.resumable = true
		g.mu.Unlock()
		return errReconnect

	case opInvalidSession:
		// Identify or Resume was refused; the session is gone and must be rebuilt from
		// scratch, otherwise the socket closes again on the next bad seq.
		slog.Warn("qq: session invalidated, will identify fresh", "conn", id)
		g.mu.Lock()
		g.resumable, g.sessionID, g.seq = false, "", 0
		g.mu.Unlock()
		return errReconnect

	default:
		slog.Debug("qq: unhandled opcode", "op", f.Op, "t", f.T)
		return nil
	}
}

// verdict turns a read error into what the reconnect loop should do with it. Keeping the
// close-code decisions here rather than in the reader matters because resume eligibility is
// session state: 4006/4007 mean the session id or seq is unusable, so resumable must be
// cleared before the next attempt, while 4009 (连接过期) is precisely the case where the
// docs tell us to resume with what we have.
func (g *gateway) verdict(err error) error {
	var ce *closeErr
	if !errors.As(err, &ce) {
		return err
	}

	switch ce.code {
	case 4006, 4007:
		g.mu.Lock()
		g.resumable, g.sessionID, g.seq = false, "", 0
		g.mu.Unlock()
		slog.Warn("qq: session unusable, identifying fresh", "code", ce.code)
		return errReconnect
	case 4009:
		slog.Info("qq: connection expired, resuming session", "code", ce.code)
		return errReconnect
	default:
		advice, permanent := closeCodeVerdict(ce.code)
		if permanent {
			return &permanentError{fmt.Errorf("qq: gateway closed %d (%s)", ce.code, advice)}
		}
		return errReconnect
	}
}

// readLoop decodes frames into a channel. The deadline is two beats plus headroom so a
// silent peer surfaces as an error instead of a hang.
func readLoop(conn *websocket.Conn, out chan<- frame, timeout time.Duration) error {
	for {
		conn.SetReadDeadline(time.Now().Add(timeout))
		var f frame
		if err := conn.ReadJSON(&f); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return &closeErr{code: ce.Code, text: ce.Text}
			}
			return fmt.Errorf("qq: read: %w", err)
		}
		out <- f
	}
}

// closeErr is a websocket close frame, kept as a code so the session layer can decide what
// it means without parsing error strings.
type closeErr struct {
	code int
	text string
}

func (e *closeErr) Error() string { return fmt.Sprintf("qq: gateway closed %d: %s", e.code, e.text) }

// closeCodeVerdict maps the documented close codes to advice plus whether retrying can ever
// succeed without an action outside this program.
func closeCodeVerdict(code int) (advice string, permanent bool) {
	switch code {
	case 4914, 4915:
		return "机器人已下架或已封禁，只有沙箱环境可连", true
	case 4013, 4014:
		return "intent 无效或无权限，需在开放平台控制台开通消息接收", true
	case 4010, 4011:
		return "shard 无效或过多", true
	default:
		return "按文档重连（4009 resume / 4006 identify）", false
	}
}

func sendFrame(conn *websocket.Conn, op int, d json.RawMessage) error {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(frame{Op: op, D: d})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only static payloads are marshalled here; a failure is a programming bug.
		panic("qq: marshal frame payload: " + err.Error())
	}
	return b
}
