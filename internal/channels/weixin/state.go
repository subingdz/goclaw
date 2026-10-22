package weixin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// contextTokenTTL is how long a reply reference stays usable. The protocol documentation
// states a reply is only possible while the peer has been active; keeping the token past
// that point does not fail loudly, it just produces an error from send, so the local
// expiry exists to turn "silently undeliverable" into a message the caller can act on.
const contextTokenTTL = 24 * time.Hour

// state holds the two pieces of information that make this channel survive a restart:
// the long-poll cursor, and the per-peer reply tokens. Both are per credential pair, keyed
// by accountKey, so two weixin instances cannot read each other's state.
type state struct {
	dir string
	key string

	mu          sync.Mutex
	cursor      string
	tokens      map[string]time.Time // from_user_id -> received at
	tokenValues map[string]string    // from_user_id -> context_token
	pausedUntil time.Time
	pauseReason string
}

func newState(dir, baseURL, token string) *state {
	k := accountKey(baseURL, token)
	return &state{
		dir:         dir,
		key:         k,
		tokens:      make(map[string]time.Time),
		tokenValues: make(map[string]string),
	}
}

// accountKeyFile is exactly 16 lowercase hex characters, which is what makes it safe to
// interpolate into a path: the state directory can never be escaped through it.
var accountKeyFile = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *state) path(kind string) string {
	if !accountKeyFile.MatchString(s.key) {
		s.key = "default"
	}
	return filepath.Clean(filepath.Join(s.dir, kind, s.key+".json"))
}

// load restores the cursor and reply tokens. Missing files are normal (first run) and are
// not errors; an unreadable file is reported because continuing without a cursor means the
// peer's history may be replayed or skipped, which the operator should know about.
func (s *state) load() error {
	if s.dir == "" {
		return nil
	}
	if raw, err := os.ReadFile(s.path("sync")); err == nil {
		var doc struct {
			GetUpdatesBuf string `json:"get_updates_buf"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("weixin: unreadable cursor file %s: %w", s.path("sync"), err)
		}
		s.cursor = doc.GetUpdatesBuf
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("weixin: read cursor: %w", err)
	}

	if raw, err := os.ReadFile(s.path("context-tokens")); err == nil {
		var doc struct {
			Tokens map[string]contextTokenEntry `json:"tokens"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("weixin: unreadable context-token file %s: %w", s.path("context-tokens"), err)
		}
		now := time.Now()
		for user, entry := range doc.Tokens {
			// Tokens written by an older run carry a timestamp; anything without one, or
			// older than the TTL, is dropped instead of being trusted forever.
			if entry.ReceivedAt.IsZero() || now.Sub(entry.ReceivedAt) > contextTokenTTL {
				continue
			}
			s.tokens[user] = entry.ReceivedAt
			s.tokenValues[user] = entry.Token
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("weixin: read context tokens: %w", err)
	}
	return nil
}

type contextTokenEntry struct {
	Token      string    `json:"token"`
	ReceivedAt time.Time `json:"received_at"`
}

func (s *state) saveCursor(cursor string) error {
	s.mu.Lock()
	s.cursor = cursor
	s.mu.Unlock()
	return s.writeJSON(s.path("sync"), struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{GetUpdatesBuf: cursor})
}

func (s *state) currentCursor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

// rememberToken records the reply reference for a peer. iLink keeps only the newest
// context per conversation, so this replaces rather than accumulates.
func (s *state) rememberToken(user, token string) error {
	if user == "" || token == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	s.tokens[user] = now
	s.tokenValues[user] = token
	dump := make(map[string]contextTokenEntry, len(s.tokenValues))
	for u, tok := range s.tokenValues {
		dump[u] = contextTokenEntry{Token: tok, ReceivedAt: s.tokens[u]}
	}
	s.mu.Unlock()
	return s.writeJSON(s.path("context-tokens"), struct {
		Tokens map[string]contextTokenEntry `json:"tokens"`
	}{Tokens: dump})
}

// replyToken returns a live reply reference for a peer, or why there is none. The reason
// matters to the caller: "never messaged" and "token expired" need different fixes.
func (s *state) replyToken(user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok, ok := s.tokenValues[user]
	if !ok || tok == "" {
		return "", fmt.Errorf("weixin: no reply reference for %s: this account can only answer a peer that has messaged it first", user)
	}
	if age := time.Since(s.tokens[user]); age > contextTokenTTL {
		return "", fmt.Errorf("weixin: reply reference for %s is %s old and has expired; the peer must send a new message", user, roundAge(age))
	}
	return tok, nil
}

// pauseSession stops using a dead session for a while instead of looping on an endpoint
// that will keep refusing it. -14 means the scan-bound session is gone; only re-activation
// fixes it, and hammering it is how an account attracts attention.
func (s *state) pauseSession(reason string, for_ time.Duration) {
	s.mu.Lock()
	s.pausedUntil = time.Now().Add(for_)
	s.pauseReason = reason
	s.mu.Unlock()
}

func (s *state) paused() (time.Duration, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := time.Until(s.pausedUntil)
	if left <= 0 {
		return 0, "", false
	}
	return left, s.pauseReason, true
}

func (s *state) writeJSON(p string, v any) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("weixin: create state dir: %w", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("weixin: encode state: %w", err)
	}
	// Write-then-rename: a crash mid-write must not leave a truncated cursor that the next
	// start would read as "no cursor" and replay the whole inbox.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("weixin: write state: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("weixin: replace state: %w", err)
	}
	return nil
}

func roundAge(d time.Duration) string {
	switch {
	case d > 48*time.Hour:
		return fmt.Sprintf("%.0f days", d.Hours()/24)
	case d > time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	default:
		return d.Round(time.Minute).String()
	}
}

// sanitizeBaseURL pins the host of a redirect returned by the QR endpoint. The value comes
// from a remote response, and following it blindly would hand this client an arbitrary
// destination, so only a weixin.qq.com host is accepted.
func sanitizeBaseURL(raw string) (string, error) {
	if raw == "" {
		return defaultBaseURL, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	if !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("weixin: redirect host %q is not https", raw)
	}
	host := strings.TrimPrefix(raw, "https://")
	host = strings.SplitN(host, "/", 2)[0]
	if !strings.HasSuffix(host, ".weixin.qq.com") {
		return "", fmt.Errorf("weixin: refusing to follow redirect to %q: only *.weixin.qq.com hosts are allowed", host)
	}
	return raw, nil
}
