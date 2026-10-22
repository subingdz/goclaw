package cmd

import (
	"context"
	"maps"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type captureSystemConfigStore struct {
	data map[string]string
}

func (s *captureSystemConfigStore) Get(_ context.Context, key string) (string, error) {
	return s.data[key], nil
}

func (s *captureSystemConfigStore) Set(_ context.Context, key, value string) error {
	s.data[key] = value
	return nil
}

func (s *captureSystemConfigStore) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}

func (s *captureSystemConfigStore) List(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.data))
	maps.Copy(out, s.data)
	return out, nil
}

func TestSeedConfigForContextPersistsZeroInboundDebounce(t *testing.T) {
	t.Parallel()

	sc := &captureSystemConfigStore{data: map[string]string{}}
	cfg := config.Default()
	cfg.Gateway.InboundDebounceMs = 0

	seedConfigForContext(store.WithTenantID(context.Background(), store.MasterTenantID), sc, cfg, false)

	if got := sc.data["gateway.inbound_debounce_ms"]; got != "0" {
		t.Fatalf("gateway.inbound_debounce_ms = %q, want 0", got)
	}
}

// audio.stt.silk_decoder is set from the panel like the rest of the STT keys, so the DB mirror
// has to carry it too — otherwise it is the one key a restart rebuilds from the file rather than
// from what the operator saved. api_key stays out of the plaintext mirror by design.
func TestSeedConfigForContextCarriesSTTKeys(t *testing.T) {
	t.Parallel()

	sc := &captureSystemConfigStore{data: map[string]string{}}
	cfg := config.Default()
	cfg.Audio = &config.AudioConfig{Stt: &config.AudioSTTConfig{
		Provider:    "openai_compat",
		BaseURL:     "https://api.siliconflow.cn/v1",
		APIKey:      "sk-test",
		Model:       "FunAudioLLM/SenseVoiceSmall",
		Language:    "vi",
		TimeoutMs:   30000,
		SilkDecoder: "/usr/local/bin/silk_v3_decoder",
	}}

	seedConfigForContext(store.WithTenantID(context.Background(), store.MasterTenantID), sc, cfg, false)

	wants := map[string]string{
		"audio.stt.provider":     "openai_compat",
		"audio.stt.model":        "FunAudioLLM/SenseVoiceSmall",
		"audio.stt.language":     "vi",
		"audio.stt.timeout_ms":   "30000",
		"audio.stt.silk_decoder": "/usr/local/bin/silk_v3_decoder",
	}
	for key, want := range wants {
		if got := sc.data[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := sc.data["audio.stt.api_key"]; ok {
		t.Fatal("audio.stt.api_key was written into the plaintext system_configs mirror")
	}
}

func TestSeedConfigForContextDoesNotCreateSkillUploadTenantOverride(t *testing.T) {
	t.Parallel()

	sc := &captureSystemConfigStore{data: map[string]string{}}
	cfg := config.Default()
	cfg.Skills.MaxUploadSizeMB = 64

	seedConfigForContext(store.WithTenantID(context.Background(), store.MasterTenantID), sc, cfg, false)

	if _, ok := sc.data[config.SkillMaxUploadSizeSystemConfigKey]; ok {
		t.Fatalf("%s should not be seeded; missing key lets SKILL.md frontmatter override global config", config.SkillMaxUploadSizeSystemConfigKey)
	}
}
