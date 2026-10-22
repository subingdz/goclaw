package weixin

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeQR serves the two unauthenticated QR endpoints. Statuses are consumed in order and
// the last one repeats, so a test can say "wait, wait, confirmed" without counting polls.
type fakeQR struct {
	mu        sync.Mutex
	statuses  []string
	botID     string
	rawBody   string // when set, every status call answers this body verbatim
	nextIdx   int
	queries   []string
	failAfter int // number of status calls that answer 500 before the list resumes
	failCount int
}

func (f *fakeQR) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/ilink/bot/get_bot_qrcode":
		if r.Header.Get("iLink-App-Id") != "bot" {
			http.Error(w, "missing app id", http.StatusBadRequest)
			return
		}
		f.queries = append(f.queries, r.URL.Query().Encode())
		_, _ = w.Write([]byte(`{"qrcode":"QR-TOKEN","qrcode_img_content":"https://ilinkai.weixin.qq.com/scan?k=QR-TOKEN"}`))

	case "/ilink/bot/get_qrcode_status":
		f.queries = append(f.queries, r.URL.Query().Encode())
		if f.rawBody != "" {
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		if f.failAfter > 0 && f.failCount < f.failAfter {
			f.failCount++
			http.Error(w, `{"ret":-1,"errmsg":"temporary"}`, http.StatusInternalServerError)
			return
		}
		idx := f.nextIdx
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		f.nextIdx++
		switch f.statuses[idx] {
		case statusConfirmed:
			_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"TOK-1","ilink_bot_id":"` + f.botID + `","ilink_user_id":"U-1","baseurl":"https://ilinkai.weixin.qq.com/"}`))
		case statusScannedRedirect:
			_, _ = w.Write([]byte(`{"status":"scaned_but_redirect"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"` + f.statuses[idx] + `"}`))
		}

	default:
		http.Error(w, `{"ret":-1,"errmsg":"unexpected path"}`, http.StatusNotFound)
	}
}

func newQRServer(t *testing.T, statuses ...string) (*httptest.Server, *fakeQR) {
	t.Helper()
	fake := &fakeQR{statuses: statuses, botID: "BOT-1"}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv, fake
}

// recordedQREvents is the emit surface captured for assertions.
type recordedQREvents struct {
	mu       sync.Mutex
	pngs     []string
	statuses []string
	dones    []qrDoneRecord
}

type qrDoneRecord struct {
	success         bool
	reason, message string
	botID           string
}

func (r *recordedQREvents) code(pngB64 string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pngs = append(r.pngs, pngB64)
}

func (r *recordedQREvents) status(status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, status)
}

func (r *recordedQREvents) done(success bool, reason, errMsg, botID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dones = append(r.dones, qrDoneRecord{success: success, reason: reason, message: errMsg, botID: botID})
}

func (r *recordedQREvents) snapshot() (pngs, statuses []string, dones []qrDoneRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pngs...), append([]string(nil), r.statuses...), append([]qrDoneRecord(nil), r.dones...)
}

func startQRTestActivation(t *testing.T, baseURL string) *Activation {
	t.Helper()
	act, err := StartActivation(context.Background(), baseURL, "")
	if err != nil {
		t.Fatalf("StartActivation() error = %v", err)
	}
	return act
}

func TestDriveActivationConfirmedStoresToken(t *testing.T) {
	srv, fake := newQRServer(t, statusWait, statusScanned, statusConfirmed)
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	creds, ok := driveActivation(context.Background(), act, ev, 5*time.Millisecond)
	if !ok {
		t.Fatalf("driveActivation() = not confirmed, dones=%+v", ev.dones)
	}
	if creds.Token != "TOK-1" || creds.IlinkBotID != "BOT-1" || creds.IlinkUserID != "U-1" {
		t.Fatalf("credentials = %+v, want the token, bot id and user id iLink returned", creds)
	}

	pngs, statuses, dones := ev.snapshot()
	if len(pngs) != 1 {
		t.Fatalf("code events = %d, want exactly one QR image", len(pngs))
	}
	if _, err := base64.StdEncoding.DecodeString(pngs[0]); err != nil {
		t.Fatalf("png_b64 is not base64: %v", err)
	}
	if len(statuses) != 1 || statuses[0] != statusScanned {
		t.Fatalf("status events = %v, want one \"scaned\"", statuses)
	}
	if len(dones) != 0 {
		t.Fatalf("done events = %+v, want none on success here", dones)
	}
	if fake.queries[1] != "qrcode=QR-TOKEN" {
		t.Fatalf("status query = %q, want the qrcode returned by get_bot_qrcode", fake.queries[1])
	}
}

// The scan feedback must not repeat: two seconds of polling while the human confirms on the
// phone would otherwise emit dozens of identical events.
func TestDriveActivationAnnouncesScanedOnce(t *testing.T) {
	srv, _ := newQRServer(t, statusWait, statusScanned)
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	t.Cleanup(cancel)

	_, ok := driveActivation(ctx, act, ev, 5*time.Millisecond)
	if ok {
		t.Fatal("driveActivation() confirmed a scan that never happened")
	}
	_, statuses, dones := ev.snapshot()
	if len(statuses) != 1 {
		t.Fatalf("status events = %v, want exactly one", statuses)
	}
	if len(dones) != 1 || dones[0].success {
		t.Fatalf("done events = %+v, want one failure", dones)
	}
	if dones[0].reason != qrReasonTimeout {
		t.Fatalf("reason = %q, want %q", dones[0].reason, qrReasonTimeout)
	}
}

func TestDriveActivationExpiredEndsSession(t *testing.T) {
	srv, _ := newQRServer(t, statusExpired)
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	if _, ok := driveActivation(context.Background(), act, ev, 5*time.Millisecond); ok {
		t.Fatal("driveActivation() confirmed an expired code")
	}
	_, _, dones := ev.snapshot()
	if len(dones) != 1 || dones[0].reason != qrReasonExpired {
		t.Fatalf("done events = %+v, want one expired failure", dones)
	}
}

// A status endpoint that keeps failing must stop eventually rather than sit on the session
// timeout pretending the code is still live.
func TestDriveActivationGivesUpAfterRepeatedPollFailures(t *testing.T) {
	srv, fake := newQRServer(t, statusWait)
	fake.failAfter = 100
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	if _, ok := driveActivation(context.Background(), act, ev, time.Millisecond); ok {
		t.Fatal("driveActivation() succeeded against a failing endpoint")
	}
	_, _, dones := ev.snapshot()
	if len(dones) != 1 || dones[0].reason != qrReasonNetwork {
		t.Fatalf("done events = %+v, want one network failure", dones)
	}
	if fake.failCount != qrMaxPollFails {
		t.Fatalf("status calls = %d, want the loop to stop at %d", fake.failCount, qrMaxPollFails)
	}
}

// Transient failures are not terminal — the human may already be holding the phone.
func TestDriveActivationToleratesTransientPollFailure(t *testing.T) {
	srv, fake := newQRServer(t, statusWait, statusConfirmed)
	fake.failAfter = 1
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	if _, ok := driveActivation(context.Background(), act, ev, time.Millisecond); !ok {
		_, _, dones := ev.snapshot()
		t.Fatalf("driveActivation() aborted on one failed poll: %+v", dones)
	}
}

// A redirect without a host means nothing moved; jumping to the official root host would
// drop an account that had already been routed elsewhere.
func TestActivationRedirectWithoutHostKeepsCurrentClient(t *testing.T) {
	srv, _ := newQRServer(t, statusScannedRedirect)
	act := startQRTestActivation(t, srv.URL)

	if err := act.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if act.Status != statusScannedRedirect {
		t.Fatalf("status = %q, want the redirect state reported unchanged", act.Status)
	}
	if act.BaseURL != srv.URL+"/" {
		t.Fatalf("base URL = %q, want the test server this flow started on", act.BaseURL)
	}
}

// A redirect host inside a remote response is not trusted: refusing it ends the session with
// the refusal visible, rather than looping on a host we will never talk to.
func TestDriveActivationRefusesForeignRedirectHost(t *testing.T) {
	srv, fake := newQRServer(t, statusWait)
	fake.rawBody = `{"status":"scaned_but_redirect","redirect_host":"evil.example.com"}`
	ev := &recordedQREvents{}

	act := startQRTestActivation(t, srv.URL)
	if _, ok := driveActivation(context.Background(), act, ev, time.Millisecond); ok {
		t.Fatal("driveActivation() followed a redirect outside weixin.qq.com")
	}
	_, _, dones := ev.snapshot()
	if len(dones) != 1 {
		t.Fatalf("done events = %+v, want exactly one", dones)
	}
	if dones[0].reason != qrReasonError {
		t.Fatalf("reason = %q, want %q", dones[0].reason, qrReasonError)
	}
	if !strings.Contains(dones[0].message, "evil.example.com") {
		t.Fatalf("message = %q, want it to name the refused host", dones[0].message)
	}
}

func TestQRPNGBase64IsDecodablePNG(t *testing.T) {
	b64, err := qrPNGBase64("https://ilinkai.weixin.qq.com/scan?k=a-long-enough-payload-to-check-the-encoder")
	if err != nil {
		t.Fatalf("qrPNGBase64() error = %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("output is not base64: %v", err)
	}
	if string(raw[1:4]) != "PNG" {
		t.Fatalf("payload is not a PNG: %q", raw[:8])
	}
}

type recordingInstanceStore struct {
	store.ChannelInstanceStore
	updatedID uuid.UUID
	updates   map[string]any
	err       error
}

func (s *recordingInstanceStore) Update(_ context.Context, id uuid.UUID, updates map[string]any) error {
	if s.err != nil {
		return s.err
	}
	s.updatedID = id
	s.updates = updates
	return nil
}

func TestPersistCredentialsWritesScannedToken(t *testing.T) {
	id := uuid.New()
	rec := &recordingInstanceStore{}
	m := &QRMethods{instanceStore: rec}

	if err := m.persistCredentials(context.Background(), id, Credentials{
		Token: "TOK-1", IlinkBotID: "BOT-1", IlinkUserID: "U-1",
	}); err != nil {
		t.Fatalf("persistCredentials() error = %v", err)
	}
	if rec.updatedID != id {
		t.Fatalf("updated id = %s, want %s", rec.updatedID, id)
	}
	creds, ok := rec.updates["credentials"].(map[string]any)
	if !ok {
		t.Fatalf("credentials = %#v, want map[string]any", rec.updates["credentials"])
	}
	if creds["token"] != "TOK-1" || creds["ilink_bot_id"] != "BOT-1" || creds["ilink_user_id"] != "U-1" {
		t.Fatalf("credentials = %+v, want token/ilink_bot_id/ilink_user_id", creds)
	}
}

func TestPersistCredentialsFailsLoudlyWithoutStore(t *testing.T) {
	m := &QRMethods{}
	err := m.persistCredentials(context.Background(), uuid.New(), Credentials{Token: "TOK"})
	if err == nil {
		t.Fatal("persistCredentials() = nil, want an error when nowhere to write")
	}
	if !strings.Contains(err.Error(), "no instance store") {
		t.Fatalf("error = %v, want it to name the missing store", err)
	}
}

func TestActivationTTLIsShorterThanSessionTimeout(t *testing.T) {
	// driveActivation distinguishes "expired" from "timeout" by which clock ran out first; if
	// the display window were longer than the session, an expired code would report a timeout.
	if activationTTL >= qrSessionTimeout {
		t.Fatalf("activationTTL=%s must be shorter than qrSessionTimeout=%s", activationTTL, qrSessionTimeout)
	}
}
