package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

func newSTTTestConfig() *config.Config {
	cfg := config.Default()
	cfg.Tts = config.TtsConfig{}
	cfg.Audio = nil
	return cfg
}

// The historical sources must keep working: a deployment that configured STT through
// tts.openai_compat transcribes voice today, and an upgrade cannot silently stop it.
func TestRegisterSTTProvidersLegacyPath(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Tts.OpenAICompat.APIBase = "http://whisper.lan:8000/v1"
	cfg.Tts.OpenAICompat.STTModel = "whisper-1"
	mgr := audio.NewManager(audio.ManagerConfig{})

	registerSTTProviders(cfg, mgr, "", "")

	if !mgr.STTAvailable(context.Background()) {
		t.Fatal("legacy tts.openai_compat endpoint registered no STT provider")
	}
}

func TestRegisterSTTProvidersFromAudioBlock(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{
		Provider:  sttProviderOpenAICompat,
		BaseURL:   "https://api.siliconflow.cn/v1",
		APIKey:    "sk-test",
		Model:     "FunAudioLLM/SenseVoiceSmall",
		Language:  "zh",
		TimeoutMs: 20000,
	}}
	mgr := audio.NewManager(audio.ManagerConfig{})

	registerSTTProviders(cfg, mgr, "", "")

	if !mgr.STTAvailable(context.Background()) {
		t.Fatal("audio.stt.provider configured no STT provider")
	}
}

// audio.stt wins over the TTS block rather than merging with it: two sources of truth
// for one chain is how a deployment ends up calling an endpoint nobody configured.
func TestRegisterSTTProvidersAudioBlockReplacesLegacy(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Tts.OpenAICompat.APIBase = "http://whisper.lan:8000/v1"
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{
		Provider: sttProviderElevenLabs,
		APIKey:   "ell-key",
	}}
	mgr := audio.NewManager(audio.ManagerConfig{})

	registerSTTProviders(cfg, mgr, "", "")

	if !mgr.STTAvailable(context.Background()) {
		t.Fatal("elevenlabs provider did not register")
	}
	// resolveSTTChain filters unregistered names, so an unwanted second entry from the
	// legacy path would be visible here as an extra provider that gets called.
	if got := mgr.STTChain(context.Background()); len(got) != 1 || got[0] != sttProviderElevenLabs {
		t.Fatalf("chain = %v, want only the configured primary", got)
	}
}

// A provider name nobody implemented must leave the gateway in "no STT" rather than
// half-configuring a chain that fails per message.
func TestRegisterSTTProvidersUnknownNameRegistersNothing(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Tts.OpenAICompat.APIBase = "http://whisper.lan:8000/v1"
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{Provider: "whisper-cloud"}}
	mgr := audio.NewManager(audio.ManagerConfig{})

	registerSTTProviders(cfg, mgr, "", "")

	if mgr.STTAvailable(context.Background()) {
		t.Fatal("an unknown provider name must not register anything, and must not fall back")
	}
}

func TestRegisterSTTProvidersElevenLabsReusesTTSKey(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{Provider: sttProviderElevenLabs}}
	mgr := audio.NewManager(audio.ManagerConfig{})

	registerSTTProviders(cfg, mgr, "elevenlabs-tts-key", "")

	if !mgr.STTAvailable(context.Background()) {
		t.Fatal("elevenlabs STT should reuse the TTS credential when audio.stt.api_key is empty")
	}
}

func TestRegisterSTTProvidersProxyNeedsEndpoint(t *testing.T) {
	mgr := audio.NewManager(audio.ManagerConfig{})
	cfg := newSTTTestConfig()
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{Provider: sttProviderProxy}}

	registerSTTProviders(cfg, mgr, "", "")
	if mgr.STTAvailable(context.Background()) {
		t.Fatal("proxy without base_url cannot serve anything")
	}
}

func TestSTTChainNames(t *testing.T) {
	tests := []struct {
		name string
		st   config.AudioSTTConfig
		want []string
	}{
		{"primary only", config.AudioSTTConfig{Provider: "openai_compat"}, []string{"openai_compat"}},
		{"primary and fallback", config.AudioSTTConfig{Provider: "openai_compat", Fallback: "elevenlabs"}, []string{"openai_compat", "elevenlabs"}},
		{"fallback equal to primary is not repeated", config.AudioSTTConfig{Provider: "openai_compat", Fallback: "openai_compat"}, []string{"openai_compat"}},
		{"blank primary leaves nothing to build", config.AudioSTTConfig{Fallback: "elevenlabs"}, nil},
		{"surrounding spaces are trimmed", config.AudioSTTConfig{Provider: "  proxy  "}, []string{"proxy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sttChainNames(&tt.st)
			if len(got) != len(tt.want) {
				t.Fatalf("chain = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("chain = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// audio.stt.fallback alone is not a backend: the primary decides, and a fallback with
// nothing to fall back from must leave the legacy wiring in charge.
func TestSTTChainNamesWithoutPrimary(t *testing.T) {
	st := &config.AudioSTTConfig{Fallback: "elevenlabs"}
	if got := sttChainNames(st); len(got) != 0 {
		t.Fatalf("chain = %v, want none", got)
	}

	cfg := newSTTTestConfig()
	cfg.Tts.OpenAICompat.APIBase = "http://whisper.lan:8000/v1"
	cfg.Audio = &config.AudioConfig{Stt: st}
	mgr := audio.NewManager(audio.ManagerConfig{})
	registerSTTProviders(cfg, mgr, "", "")
	if got := mgr.STTChain(context.Background()); len(got) != 1 || got[0] != sttProviderOpenAICompat {
		t.Fatalf("chain = %v, want the legacy openai_compat endpoint to remain in charge", got)
	}
}

func TestSTTTimeoutSeconds(t *testing.T) {
	tests := []struct {
		ms   int
		want int
	}{
		{0, 0},  // provider default
		{-5, 0}, // never a negative timeout
		{1, 1},  // a sub-second timeout must not become 0
		{20000, 20},
		{30500, 31},
	}
	for _, tt := range tests {
		if got := sttTimeoutSeconds(tt.ms); got != tt.want {
			t.Fatalf("sttTimeoutSeconds(%d) = %d, want %d", tt.ms, got, tt.want)
		}
	}
}

// The startup log line is the only place an operator can see that the fallback registered at
// all. The role used to be read after the entry was appended to the chain, which made every
// line answer "primary" and hid the second provider.
func TestRegisterSTTProvidersLogsChainRoles(t *testing.T) {
	cfg := newSTTTestConfig()
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{
		Provider: sttProviderOpenAICompat,
		Fallback: sttProviderProxy,
		BaseURL:  "https://api.siliconflow.cn/v1",
		APIKey:   "sk-test",
		Model:    "FunAudioLLM/SenseVoiceSmall",
	}}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mgr := audio.NewManager(audio.ManagerConfig{})
	registerSTTProviders(cfg, mgr, "", "")

	chain := mgr.STTChain(context.Background())
	if len(chain) != 2 || chain[0] != sttProviderOpenAICompat || chain[1] != sttProviderProxy {
		t.Fatalf("chain = %v, want [%s %s]", chain, sttProviderOpenAICompat, sttProviderProxy)
	}

	out := buf.String()
	if !strings.Contains(out, "provider="+sttProviderOpenAICompat+" role=primary") {
		t.Fatalf("the primary was not logged as primary:\n%s", out)
	}
	if !strings.Contains(out, "provider="+sttProviderProxy+" role=fallback") {
		t.Fatalf("the fallback was not logged as fallback:\n%s", out)
	}
}
