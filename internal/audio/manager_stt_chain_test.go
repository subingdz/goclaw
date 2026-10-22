package audio

import (
	"context"
	"errors"
	"testing"
)

// A chain that names a provider which is only registered per channel must resolve to
// the registered entries, not walk the bare name on every voice message. That was the
// old behavior: cmd appended "proxy" unconditionally, and "proxy" is only ever stored
// as "proxy:telegram".
func TestResolveSTTChain_FiltersUnregisteredNames(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{name: "openai_compat", result: &TranscriptResult{Text: "ok"}})
	m.SetSTTChain([]string{"openai_compat", "proxy"})

	if got := m.resolveSTTChain(context.Background()); len(got) != 1 || got[0] != "openai_compat" {
		t.Fatalf("chain = %v, want only the registered openai_compat", got)
	}
	res, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("text = %q, want the registered provider's result", res.Text)
	}
}

// A chain of nothing but unregistered names is a configuration error, not a caller
// error: it must surface as the empty-chain error rather than silently trying anything.
func TestResolveSTTChain_AllUnregisteredIsEmpty(t *testing.T) {
	m := newTestManager()
	m.RegisterSTT(&mockSTT{name: "elevenlabs"})
	m.SetSTTChain([]string{"proxy"})

	if got := m.resolveSTTChain(context.Background()); len(got) != 0 {
		t.Fatalf("chain = %v, want empty", got)
	}
	if _, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{}); !errors.Is(err, ErrAllSTTProvidersFailed) {
		t.Fatalf("Transcribe() error = %v, want ErrAllSTTProvidersFailed", err)
	}
}

func TestSTTAvailable(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*Manager)
		ctxChan string
		want    bool
	}{
		{
			name:  "nothing configured",
			setup: func(*Manager) {},
			want:  false,
		},
		{
			name: "global provider",
			setup: func(m *Manager) {
				m.RegisterSTT(&mockSTT{name: "elevenlabs"})
				m.SetSTTChain([]string{"elevenlabs"})
			},
			want: true,
		},
		{
			name: "channel override reaches its own channel",
			setup: func(m *Manager) {
				m.RegisterChannelSTT("feishu", &mockSTT{name: "proxy"})
			},
			ctxChan: "feishu",
			want:    true,
		},
		{
			name: "another channel does not inherit the override",
			setup: func(m *Manager) {
				m.RegisterChannelSTT("feishu", &mockSTT{name: "proxy"})
			},
			ctxChan: "discord",
			want:    false,
		},
		{
			name: "default chain provider counts without an explicit chain",
			setup: func(m *Manager) {
				m.RegisterSTT(&mockSTT{name: "elevenlabs"})
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager()
			tt.setup(m)
			ctx := context.Background()
			if tt.ctxChan != "" {
				ctx = WithChannel(ctx, tt.ctxChan)
			}
			if got := m.STTAvailable(ctx); got != tt.want {
				t.Fatalf("STTAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Channels pass an empty STTOptions because they cannot know the operator's model or
// language; the configured defaults have to reach the provider, and an explicit
// per-call value must still win.
func TestTranscribe_AppliesConfiguredDefaults(t *testing.T) {
	capture := &capturingSTT{name: "capture", result: &TranscriptResult{Text: "x"}}
	m := newTestManager()
	m.RegisterSTT(capture)
	m.SetSTTChain([]string{capture.name})
	m.SetSTTDefaults(STTOptions{Language: "zh", ModelID: "SenseVoiceSmall", TimeoutMs: 15000})

	if _, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if capture.got.Language != "zh" || capture.got.ModelID != "SenseVoiceSmall" || capture.got.TimeoutMs != 15000 {
		t.Fatalf("opts = %+v, want the configured defaults", capture.got)
	}

	if _, err := m.Transcribe(context.Background(), STTInput{}, STTOptions{Language: "vi"}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if capture.got.Language != "vi" || capture.got.ModelID != "SenseVoiceSmall" {
		t.Fatalf("opts = %+v, want the per-call language to win and the default model to stay", capture.got)
	}
}

type capturingSTT struct {
	name   string
	result *TranscriptResult
	got    STTOptions
}

func (c *capturingSTT) Name() string { return c.name }

func (c *capturingSTT) Transcribe(_ context.Context, _ STTInput, opts STTOptions) (*TranscriptResult, error) {
	c.got = opts
	return c.result, nil
}
