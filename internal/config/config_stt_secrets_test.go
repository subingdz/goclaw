package config

import "testing"

// The STT key is a secret like every other one: masked on the way out to the panel,
// never written to config.json, and round-tripped through the config_secrets table so
// a panel-set value survives a restart.
func TestSTTAPIKeySecretRoundTrip(t *testing.T) {
	cfg := Default()
	st := cfg.ensureAudioSTT()
	st.Provider = "openai_compat"
	st.BaseURL = "https://api.siliconflow.cn/v1"
	st.APIKey = "sk-secret"

	if got := cfg.MaskedCopy().Audio.Stt.APIKey; got != secretMask {
		t.Fatalf("MaskedCopy leaked the STT key: %q", got)
	}
	// The non-secret endpoint stays visible — the panel needs it to render the form.
	if got := cfg.MaskedCopy().Audio.Stt.BaseURL; got != st.BaseURL {
		t.Fatalf("MaskedCopy dropped base_url: %q", got)
	}

	collected := cfg.ExtractDBSecrets()
	if collected["audio.stt.api_key"] != "sk-secret" {
		t.Fatalf("ExtractDBSecrets = %v, want audio.stt.api_key", collected)
	}

	stripped := Default()
	sp := stripped.ensureAudioSTT()
	sp.APIKey = "sk-secret"
	sp.Provider = "openai_compat"
	sp.BaseURL = "https://api.siliconflow.cn/v1"
	stripped.StripSecrets()
	if stripped.Audio.Stt.APIKey != "" {
		t.Fatalf("StripSecrets left %q on the disk-bound copy", stripped.Audio.Stt.APIKey)
	}
	if stripped.Audio.Stt.Provider != "openai_compat" {
		t.Fatal("StripSecrets must not touch non-secret settings")
	}

	// A config.json with no audio block at all must still receive the stored secret.
	fresh := Default()
	fresh.ApplyDBSecrets(map[string]string{"audio.stt.api_key": "sk-from-db"})
	if fresh.Audio == nil || fresh.Audio.Stt == nil || fresh.Audio.Stt.APIKey != "sk-from-db" {
		t.Fatalf("ApplyDBSecrets did not restore the STT key: %#v", fresh.Audio)
	}

	// The mask is what the panel echoes back when the operator changed something else.
	masked := Default()
	mp := masked.ensureAudioSTT()
	mp.APIKey = secretMask
	masked.StripMaskedSecrets()
	if masked.Audio.Stt.APIKey != "" {
		t.Fatalf("StripMaskedSecrets kept the mask literal: %q", masked.Audio.Stt.APIKey)
	}
}

func TestApplySystemConfigsSTT(t *testing.T) {
	cfg := Default()
	cfg.ApplySystemConfigs(map[string]string{
		"audio.stt.provider":   "openai_compat",
		"audio.stt.base_url":   "https://api.siliconflow.cn/v1",
		"audio.stt.model":      "FunAudioLLM/SenseVoiceSmall",
		"audio.stt.language":   "zh",
		"audio.stt.fallback":   "elevenlabs",
		"audio.stt.timeout_ms": "20000",
	})
	st := cfg.Audio.Stt
	if st.Provider != "openai_compat" || st.BaseURL != "https://api.siliconflow.cn/v1" ||
		st.Model != "FunAudioLLM/SenseVoiceSmall" || st.Language != "zh" ||
		st.Fallback != "elevenlabs" || st.TimeoutMs != 20000 {
		t.Fatalf("audio.stt overlay = %+v", st)
	}
}

// The endpoint is config; the key is a secret. Overlaying the plaintext map must never
// pick up a key from it.
func TestApplySystemConfigsIgnoresSTTAPIKey(t *testing.T) {
	cfg := Default()
	cfg.ApplySystemConfigs(map[string]string{"audio.stt.api_key": "sk-should-be-ignored"})
	if cfg.Audio == nil || cfg.Audio.Stt == nil {
		t.Fatal("expected the audio block to exist after overlay")
	}
	if cfg.Audio.Stt.APIKey != "" {
		t.Fatalf("api_key was read from system_configs: %q", cfg.Audio.Stt.APIKey)
	}
}

func TestEnsureAudioSTTIsIdempotent(t *testing.T) {
	cfg := Default()
	first := cfg.ensureAudioSTT()
	first.Provider = "proxy"
	if got := cfg.ensureAudioSTT(); got != first || got.Provider != "proxy" {
		t.Fatalf("ensureAudioSTT allocated twice: %#v", got)
	}
}
