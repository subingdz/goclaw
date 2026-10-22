package audio

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mockSTT is a test STTProvider that either returns a fixed result or error.
// gotInput keeps the most recent input so a test can assert what the manager actually
// handed the provider — for example a decoded WAV where a SILK file went in.
type mockSTT struct {
	name     string
	result   *TranscriptResult
	err      error
	gotInput STTInput
}

func (m *mockSTT) Name() string { return m.name }

func (m *mockSTT) Transcribe(_ context.Context, in STTInput, _ STTOptions) (*TranscriptResult, error) {
	m.gotInput = in
	return m.result, m.err
}

func newTestManager() *Manager {
	return NewManager(ManagerConfig{})
}

// Case 1: chain ["elevenlabs","proxy"] — Scribe succeeds, result.Provider="elevenlabs".
func TestManager_Transcribe_ChainSuccessFirst(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{
		name:   "elevenlabs",
		result: &TranscriptResult{Text: "hello", Provider: "elevenlabs"},
	})
	m.RegisterSTT(&mockSTT{
		name:   "proxy",
		result: &TranscriptResult{Text: "world", Provider: "proxy"},
	})
	m.SetSTTChain([]string{"elevenlabs", "proxy"})

	res, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Provider != "elevenlabs" {
		t.Errorf("expected provider 'elevenlabs', got %q", res.Provider)
	}
	if res.Text != "hello" {
		t.Errorf("expected text 'hello', got %q", res.Text)
	}
}

// Case 2: Scribe fails → proxy succeeds → result.Provider="proxy".
func TestManager_Transcribe_FallsToProxy(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{
		name: "elevenlabs",
		err:  errors.New("scribe 500"),
	})
	m.RegisterSTT(&mockSTT{
		name:   "proxy",
		result: &TranscriptResult{Text: "fallback", Provider: "proxy"},
	})
	m.SetSTTChain([]string{"elevenlabs", "proxy"})

	res, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Provider != "proxy" {
		t.Errorf("expected provider 'proxy', got %q", res.Provider)
	}
}

// Case 3: both fail → ErrAllSTTProvidersFailed.
func TestManager_Transcribe_AllFail(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{name: "elevenlabs", err: errors.New("scribe error")})
	m.RegisterSTT(&mockSTT{name: "proxy", err: errors.New("proxy error")})
	m.SetSTTChain([]string{"elevenlabs", "proxy"})

	_, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrAllSTTProvidersFailed) {
		t.Errorf("expected ErrAllSTTProvidersFailed, got: %v", err)
	}
}

// Case 4: empty chain → error.
func TestManager_Transcribe_EmptyChain(t *testing.T) {
	m := newTestManager()
	m.SetSTTChain([]string{})

	_, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err == nil {
		t.Fatal("expected error for empty chain, got nil")
	}
	if !errors.Is(err, ErrAllSTTProvidersFailed) {
		t.Errorf("expected ErrAllSTTProvidersFailed, got: %v", err)
	}
}

// Case 5: unknown provider in chain is skipped with warn; known one succeeds.
func TestManager_Transcribe_UnknownSkipped(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{
		name:   "proxy",
		result: &TranscriptResult{Text: "ok", Provider: "proxy"},
	})
	m.SetSTTChain([]string{"unknown_provider", "proxy"})

	res, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Provider != "proxy" {
		t.Errorf("expected provider 'proxy', got %q", res.Provider)
	}
}

// Case 6: channel override wins over tenant chain.
func TestManager_Transcribe_ChannelOverrideWins(t *testing.T) {
	m := newTestManager()

	// Register tenant-level provider.
	m.RegisterSTT(&mockSTT{
		name:   "elevenlabs",
		result: &TranscriptResult{Text: "tenant", Provider: "elevenlabs"},
	})
	m.SetSTTChain([]string{"elevenlabs"})

	// Register channel-scoped proxy that wins for "telegram".
	channelProxy := &mockSTT{
		name:   "proxy",
		result: &TranscriptResult{Text: "channel-override", Provider: "proxy"},
	}
	m.RegisterChannelSTT("telegram", channelProxy)

	ctx := WithChannel(context.Background(), "telegram")
	res, err := m.Transcribe(ctx, STTInput{}, STTOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "channel-override" {
		t.Errorf("expected channel override result, got %q", res.Text)
	}
}

// STTTimeout is how audio.stt.timeout_ms reaches a channel's call deadline. Channels used to
// hard-code theirs, so any configured value above 10s was ignored in silence — including the
// long voice notes an operator raises the timeout for.
func TestSTTTimeoutHonoursConfiguredValue(t *testing.T) {
	const channelDefault = 10 * time.Second

	m := NewManager(ManagerConfig{})
	if got := m.STTTimeout(channelDefault); got != channelDefault {
		t.Fatalf("unset timeout = %v, want the channel default %v", got, channelDefault)
	}

	m.SetSTTDefaults(STTOptions{TimeoutMs: 30000})
	if got := m.STTTimeout(channelDefault); got != 30*time.Second {
		t.Fatalf("configured timeout = %v, want 30s: the operator's number must win", got)
	}

	m.SetSTTDefaults(STTOptions{TimeoutMs: 4000})
	if got := m.STTTimeout(channelDefault); got != 4*time.Second {
		t.Fatalf("configured timeout = %v, want the tighter 4s", got)
	}

	// A zero timeout_ms means "no opinion", not "no deadline".
	m.SetSTTDefaults(STTOptions{})
	if got := m.STTTimeout(channelDefault); got != channelDefault {
		t.Fatalf("cleared timeout = %v, want the channel default back", got)
	}

	var nilMgr *Manager
	if got := nilMgr.STTTimeout(channelDefault); got != channelDefault {
		t.Fatalf("nil manager timeout = %v, want the caller's default instead of a panic", got)
	}
}
