package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateModelRouting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		bag     string
		wantErr bool
	}{
		{name: "absent", bag: `{"prompt_mode":"full"}`},
		{name: "null", bag: `{"model_routing":null}`},
		// Parking an unfinished policy is normal; only an enabled one has to work.
		{name: "disabled saves as-is", bag: `{"model_routing":{"enabled":false}}`},
		{name: "not an object", bag: `{"model_routing":[1,2]}`, wantErr: true},
		{name: "wrong field type", bag: `{"model_routing":{"enabled":true,"tiers":[{"model":"m","min_runes":"lots"}]}}`, wantErr: true},
		{name: "tiers not a list", bag: `{"model_routing":{"enabled":true,"tiers":{"model":"m"}}}`, wantErr: true},
		{name: "enabled without tiers", bag: `{"model_routing":{"enabled":true}}`, wantErr: true},
		{name: "tiers all blank", bag: `{"model_routing":{"enabled":true,"tiers":[{"name":"oops"}]}}`, wantErr: true},
		{name: "too many tiers", bag: `{"model_routing":{"enabled":true,"tiers":[` + strings.TrimSuffix(strings.Repeat(`{"model":"m"},`, modelRoutingMaxTiers+1), ",") + `]}}`, wantErr: true},
		{name: "negative threshold", bag: `{"model_routing":{"enabled":true,"tiers":[{"model":"m","min_runes":-5}]}}`, wantErr: true},
		{name: "classifier without model", bag: `{"model_routing":{"enabled":true,"tiers":[{"model":"m"}],"classifier":{"provider":"x"}}}`, wantErr: true},
		{name: "negative classifier timeout", bag: `{"model_routing":{"enabled":true,"tiers":[{"model":"m"}],"classifier":{"model":"mini","timeout_ms":-1}}}`, wantErr: true},
		{name: "valid", bag: `{"model_routing":{"enabled":true,"tiers":[{"name":"a","model":"m-a","min_runes":400}],"classifier":{"model":"mini","timeout_ms":3000}}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bag map[string]any
			if err := json.Unmarshal([]byte(tc.bag), &bag); err != nil {
				t.Fatalf("fixture is not valid JSON: %v", err)
			}
			err := ValidateModelRouting(bag)
			if tc.wantErr && err == nil {
				t.Fatal("expected a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

// The write path refuses what the read path silently ignores — pin both so neither drifts.
func TestValidateModelRoutingMatchesParserTolerance(t *testing.T) {
	t.Parallel()

	bag := `{"model_routing":{"enabled":true,"tiers":[{"hint":"no model"},{"model":"m"}]}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(bag), &m); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModelRouting(m); err != nil {
		t.Fatalf("one usable tier should pass: %v", err)
	}
	cfg := FindModelRoutingConfig(json.RawMessage(bag))
	if cfg == nil || len(cfg.Tiers) != 1 {
		t.Fatalf("parser must drop the model-less tier, got %+v", cfg)
	}
}

func TestParseModelRoutingFromOtherConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		wantOK    bool
		wantTiers int
	}{
		{name: "empty bag", raw: ``},
		{name: "null object", raw: `{}`},
		{name: "unrelated keys", raw: `{"prompt_mode":"full","inbound_debounce_ms":500}`},
		{name: "malformed json", raw: `{"model_routing":`},
		{name: "wrong shape", raw: `{"model_routing":[1,2]}`},
		{name: "disabled", raw: `{"model_routing":{"enabled":false,"tiers":[{"model":"big"}]}}`},
		{name: "enabled no tiers", raw: `{"model_routing":{"enabled":true}}`},
		{
			name:      "enabled with one tier",
			raw:       `{"model_routing":{"enabled":true,"tiers":[{"model":"big"}]}}`,
			wantOK:    true,
			wantTiers: 1,
		},
		{
			name:      "model-less rows are skipped, rest survives",
			raw:       `{"model_routing":{"enabled":true,"tiers":[{"model":"  "},{"model":"big"}]}}`,
			wantOK:    true,
			wantTiers: 1,
		},
		{
			name:      "every tier blank disables routing",
			raw:       `{"model_routing":{"enabled":true,"tiers":[{"model":""},{"hint":"no model"}]}}`,
			wantOK:    false,
			wantTiers: 0,
		},
		{
			// The cap is a config guard: a panel bug that pasted 40 tiers must not turn the
			// ordered walk into something nobody can read.
			name:      "tier list is capped",
			raw:       `{"model_routing":{"enabled":true,"tiers":[` + strings.TrimSuffix(strings.Repeat(`{"model":"m"},`, modelRoutingMaxTiers+5), ",") + `]}}`,
			wantOK:    true,
			wantTiers: modelRoutingMaxTiers,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, ok := ParseModelRoutingFromOtherConfig(json.RawMessage(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if cfg != nil {
					t.Fatalf("cfg must be nil when ok is false, got %+v", cfg)
				}
				return
			}
			if len(cfg.Tiers) != tc.wantTiers {
				t.Fatalf("tiers = %d, want %d", len(cfg.Tiers), tc.wantTiers)
			}
		})
	}
}

func TestParseModelRoutingNormalization(t *testing.T) {
	t.Parallel()

	cfg := FindModelRoutingConfig(json.RawMessage(`{"model_routing":{
		"enabled":true,
		"tiers":[{"model":"  deepseek-v3  ","provider":" silicon ","name":"  ","hint":"cheap"}],
		"classifier":{"model":" mini ","provider":"","timeout_ms":0,"cache_ttl_seconds":0}
	}}`))
	if cfg == nil {
		t.Fatal("expected routing config")
	}
	got := cfg.Tiers[0]
	if got.Model != "deepseek-v3" || got.Provider != "silicon" {
		t.Errorf("whitespace not trimmed: %+v", got)
	}
	// An unnamed tier is named by its model so trace tags and logs stay meaningful.
	if got.Name != "deepseek-v3" {
		t.Errorf("name default = %q, want the model id", got.Name)
	}
	if cfg.Classifier == nil {
		t.Fatal("classifier expected")
	}
	if cfg.Classifier.Model != "mini" {
		t.Errorf("classifier model = %q", cfg.Classifier.Model)
	}
	if cfg.Classifier.TimeoutMs != modelRoutingDefaultClassifierTimeout {
		t.Errorf("timeout default = %d, want %d", cfg.Classifier.TimeoutMs, modelRoutingDefaultClassifierTimeout)
	}
	if cfg.Classifier.CacheTTLSeconds != modelRoutingDefaultClassifierTTL {
		t.Errorf("ttl default = %d, want %d", cfg.Classifier.CacheTTLSeconds, modelRoutingDefaultClassifierTTL)
	}
}

func TestFindModelRoutingConfigBlankClassifierIsDropped(t *testing.T) {
	t.Parallel()

	// A saved-but-empty classifier model would be paid for and answer nothing, so routing keeps
	// its tiers and loses only the classifier.
	cfg := FindModelRoutingConfig(json.RawMessage(`{"model_routing":{
		"enabled":true,
		"tiers":[{"model":"big"}],
		"classifier":{"model":"   "}
	}}`))
	if cfg == nil {
		t.Fatal("routing should stay enabled")
	}
	if cfg.Classifier != nil {
		t.Fatalf("blank classifier should be nil, got %+v", cfg.Classifier)
	}
}
