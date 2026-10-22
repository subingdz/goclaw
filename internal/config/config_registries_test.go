package config

import (
	"encoding/json"
	"testing"
)

// Community source tokens are secrets like every other one: masked on the way to the panel,
// never written to config.json, and round-tripped through config_secrets so a panel-set token
// survives a restart.
func TestRegistryTokensSecretRoundTrip(t *testing.T) {
	cfg := Default()
	regs := cfg.ensureRegistries()
	regs.ClawHub = &SkillRegistryConfig{Enabled: ptrTrue(), BaseURL: "https://clawhub.ai", Token: "hub-token"}
	regs.GitHub = &SkillRegistryConfig{Token: "gh-token"}

	if got := cfg.MaskedCopy().Skills.Registries.ClawHub.Token; got != secretMask {
		t.Fatalf("MaskedCopy leaked the clawhub token: %q", got)
	}
	if got := cfg.MaskedCopy().Skills.Registries.GitHub.Token; got != secretMask {
		t.Fatalf("MaskedCopy leaked the github token: %q", got)
	}
	// The non-secret parts stay visible so the panel can render the form.
	if got := cfg.MaskedCopy().Skills.Registries.ClawHub.BaseURL; got != "https://clawhub.ai" {
		t.Fatalf("MaskedCopy dropped base_url: %q", got)
	}

	collected := cfg.ExtractDBSecrets()
	if collected["skills.registries.clawhub.token"] != "hub-token" {
		t.Fatalf("ExtractDBSecrets = %v", collected)
	}
	if collected["skills.registries.github.token"] != "gh-token" {
		t.Fatalf("ExtractDBSecrets = %v", collected)
	}

	cfg.StripSecrets()
	if cfg.Skills.Registries.ClawHub.Token != "" || cfg.Skills.Registries.GitHub.Token != "" {
		t.Fatalf("StripSecrets left tokens on the disk copy: %+v", cfg.Skills.Registries)
	}
	if !cfg.Skills.Registries.ClawHub.IsEnabled() {
		t.Fatal("StripSecrets must not touch the enabled flag")
	}

	masked := Default()
	mr := masked.ensureRegistries()
	mr.ClawHub = &SkillRegistryConfig{Token: secretMask}
	masked.StripMaskedSecrets()
	if masked.Skills.Registries.ClawHub.Token != "" {
		t.Fatalf("StripMaskedSecrets kept the mask literal: %q", masked.Skills.Registries.ClawHub.Token)
	}

	// A config.json that never mentioned registries must still receive stored tokens.
	fresh := Default()
	fresh.ApplyDBSecrets(map[string]string{"skills.registries.github.token": "gh-from-db"})
	regs2 := fresh.Skills.Registries
	if regs2 == nil || regs2.GitHub == nil || regs2.GitHub.Token != "gh-from-db" {
		t.Fatalf("ApplyDBSecrets did not restore the github token: %#v", fresh.Skills.Registries)
	}
	if regs2.ClawHub != nil && regs2.ClawHub.IsEnabled() {
		t.Fatal("applying one token must not enable the other source")
	}
}

func TestRegistryEnabledIsExplicit(t *testing.T) {
	tests := []struct {
		name string
		cfg  *SkillRegistryConfig
		want bool
	}{
		{"nil block", nil, false},
		{"unset", &SkillRegistryConfig{}, false},
		{"explicit false", &SkillRegistryConfig{Enabled: ptrFalse()}, false},
		{"true", &SkillRegistryConfig{Enabled: ptrTrue()}, true},
		{"token only is not consent", &SkillRegistryConfig{Token: "t"}, false},
	}
	for _, tt := range tests {
		if got := tt.cfg.IsEnabled(); got != tt.want {
			t.Errorf("%s: IsEnabled() = %v, want %v", tt.name, got, tt.want)
		}
	}

	off := &SkillRegistriesConfig{ClawHub: &SkillRegistryConfig{}, GitHub: &SkillRegistryConfig{Enabled: ptrFalse()}}
	if off.AnyEnabled() {
		t.Fatal("AnyEnabled() = true with nothing enabled")
	}
	on := &SkillRegistriesConfig{ClawHub: &SkillRegistryConfig{Enabled: ptrTrue()}}
	if !on.AnyEnabled() {
		t.Fatal("AnyEnabled() = false with clawhub on")
	}
	var nilRegs *SkillRegistriesConfig
	if nilRegs.AnyEnabled() {
		t.Fatal("nil registries must not report anything enabled")
	}
}

// The block is optional: a config that never asked for community sources must not gain one, so
// an unchanged config.json keeps its shape.
func TestRegistriesOmittedWhenUnset(t *testing.T) {
	data, err := json.Marshal(Default().Skills)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic["registries"]; ok {
		t.Fatalf("Default().Skills marshalled a registries block: %s", data)
	}

	var back SkillsConfig
	if err := json.Unmarshal([]byte(`{"slash_commands":{"enabled":true}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Registries != nil {
		t.Fatalf("Registries = %+v, want nil when the file has no such block", back.Registries)
	}
}

// An optional block must only appear when something actually configured it: envStr's argument
// is evaluated eagerly, so `envStr(key, &c.ensureRegistries()...Token)` used to allocate a
// `"registries": {}` block into every saved config.json.
func TestEnvOverlayDoesNotAllocateOptionalBlocks(t *testing.T) {
	t.Setenv("GOCLAW_AUDIO_STT_API_KEY", "")
	t.Setenv("GOCLAW_SKILLS_CLAWHUB_TOKEN", "")
	t.Setenv("GOCLAW_SKILLS_GITHUB_TOKEN", "")

	cfg := Default()
	cfg.applyEnvOverrides()
	if cfg.Audio != nil {
		t.Fatalf("audio block created with no STT key in the environment: %#v", cfg.Audio)
	}
	if cfg.Skills.Registries != nil {
		t.Fatalf("registries block created with no tokens in the environment: %#v", cfg.Skills.Registries)
	}

	t.Setenv("GOCLAW_SKILLS_CLAWHUB_TOKEN", "hub-token")
	second := Default()
	second.applyEnvOverrides()
	regs := second.Skills.Registries
	if regs == nil || regs.ClawHub == nil || regs.ClawHub.Token != "hub-token" {
		t.Fatalf("the clawhub token did not land: %#v", regs)
	}
	if regs.GitHub != nil {
		t.Fatalf("a github block appeared from a clawhub-only variable: %#v", regs.GitHub)
	}
	// A token is not consent: the source still has to be enabled.
	if regs.ClawHub.IsEnabled() {
		t.Fatal("holding a token must not enable a source")
	}
}

func TestEnsureRegistriesIsIdempotent(t *testing.T) {
	cfg := Default()
	first := cfg.ensureRegistries()
	first.ClawHub = &SkillRegistryConfig{BaseURL: "https://mirror.example"}
	if got := cfg.ensureRegistries(); got != first {
		t.Fatal("ensureRegistries allocated twice")
	}
	if got := first.ensureClawHub(); got != first.ClawHub {
		t.Fatal("ensureClawHub replaced an existing block")
	}
	gh := first.ensureGitHub()
	if first.ensureGitHub() != gh {
		t.Fatal("ensureGitHub allocated twice")
	}
}

func ptrTrue() *bool  { v := true; return &v }
func ptrFalse() *bool { v := false; return &v }
