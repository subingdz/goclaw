package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Model routing lives in the agents.other_config bag (see agent_store.go's
// ParseAllowImageGeneration / ParseInboundDebounceMs precedents) precisely so that adding it
// needs no PostgreSQL migration and no SQLite schema patch. It is *not* the same thing as
// `model_fallback`: fallback is failure-driven (rate limit, overload, transport), this is
// request-driven and runs before any provider call.
//
// GoClaw does not know which model is "stronger" — no provider publishes that — so tiers are an
// ordered list and the first matching one wins. That ordering *is* the policy: put the expensive
// tier first with high thresholds, and a cheap tier last if you want downgrade-on-everything-else.

// ModelRoutingTier is one destination: a model, optionally on another provider, plus the
// conditions that make it the right choice for this turn.
type ModelRoutingTier struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"` // empty = the agent's own provider
	// Hint describes the tier in one line for the operator and, when the classifier is on,
	// for the classifying model. It is the only place "what is this tier for" is recorded.
	Hint string `json:"hint,omitempty"`

	// Thresholds. All declared ones must hold for the tier to match (AND), and a zero value
	// means "not a condition", so an all-zero tier matches every turn.
	MinRunes      int      `json:"min_runes,omitempty"`
	MinTokens     int      `json:"min_tokens,omitempty"`
	RequiresCode  bool     `json:"requires_code,omitempty"`
	RequiresMedia bool     `json:"requires_media,omitempty"`
	RunKinds      []string `json:"run_kinds,omitempty"` // "user", "delegation", "announce"; empty = any
}

// ModelRoutingClassifier configures the optional "ask a cheap model how hard this is" step.
// It is opt-in because it costs a call: rules answer first, and the classifier only runs when
// no rule matched.
type ModelRoutingClassifier struct {
	Model           string `json:"model"`
	Provider        string `json:"provider,omitempty"`
	TimeoutMs       int    `json:"timeout_ms,omitempty"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds,omitempty"`
}

// ModelRoutingConfig is the decoded `other_config.model_routing` object.
type ModelRoutingConfig struct {
	Enabled    bool                    `json:"enabled"`
	Tiers      []ModelRoutingTier      `json:"tiers,omitempty"`
	Classifier *ModelRoutingClassifier `json:"classifier,omitempty"`
}

const (
	// modelRoutingMaxTiers bounds the config, not the ambition: an ordered list a human can
	// read is the point. Beyond this a typo in the panel would silently reorder policy.
	modelRoutingMaxTiers = 8
	// modelRoutingDefaultClassifierTimeout keeps the extra call from dominating the turn.
	modelRoutingDefaultClassifierTimeout = 4000
	// modelRoutingDefaultClassifierTTL is short on purpose: the same message re-running within
	// a few minutes should not pay for the classifier twice, but a long cache would freeze a
	// decision against a config the operator may have just edited.
	modelRoutingDefaultClassifierTTL = 600
)

// ParseModelRoutingFromOtherConfig reads the routing bag. ok=false means "do not route": absent,
// null, malformed, disabled, or no usable tier. Every one of those is a normal state — the agent
// keeps its configured model — so nothing here logs, and the caller must not fail a run over it.
func ParseModelRoutingFromOtherConfig(raw json.RawMessage) (cfg *ModelRoutingConfig, ok bool) {
	if len(raw) <= 2 {
		return nil, false
	}
	var envelope struct {
		ModelRouting *ModelRoutingConfig `json:"model_routing"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, false
	}
	r := envelope.ModelRouting
	if r == nil || !r.Enabled || len(r.Tiers) == 0 {
		return nil, false
	}

	tiers := make([]ModelRoutingTier, 0, len(r.Tiers))
	for _, t := range r.Tiers {
		if len(tiers) >= modelRoutingMaxTiers {
			break
		}
		t.Model = strings.TrimSpace(t.Model)
		t.Provider = strings.TrimSpace(t.Provider)
		t.Name = strings.TrimSpace(t.Name)
		if t.Model == "" {
			// A tier without a model is a half-saved panel row, not a policy. Skipping it keeps
			// the rest of the list meaningful instead of disabling routing entirely.
			continue
		}
		if t.Name == "" {
			t.Name = t.Model
		}
		tiers = append(tiers, t)
	}
	if len(tiers) == 0 {
		return nil, false
	}

	r.Tiers = tiers
	if r.Classifier != nil {
		r.Classifier.Model = strings.TrimSpace(r.Classifier.Model)
		if r.Classifier.Model == "" {
			r.Classifier = nil // an empty classifier would be paid for and answer nothing
		} else {
			if r.Classifier.TimeoutMs <= 0 {
				r.Classifier.TimeoutMs = modelRoutingDefaultClassifierTimeout
			}
			if r.Classifier.CacheTTLSeconds <= 0 {
				r.Classifier.CacheTTLSeconds = modelRoutingDefaultClassifierTTL
			}
		}
	}
	return r, true
}

// FindModelRoutingConfig parses an agent's bag for callers that hold raw JSON.
func FindModelRoutingConfig(raw json.RawMessage) *ModelRoutingConfig {
	cfg, ok := ParseModelRoutingFromOtherConfig(raw)
	if !ok {
		return nil
	}
	return cfg
}

// ValidateModelRouting gates the write path for an already-decoded other_config map. It is
// deliberately stricter than the read path: ParseModelRoutingFromOtherConfig swallows anything
// malformed so a stored policy can never break a run, which means a typo saved through a raw API
// call would sit there inert and invisible. Rejecting at save time is the only place the mistake
// is still fixable.
//
// Absent or null `model_routing` is valid (routing off), and so is a disabled policy — saving one
// is how an operator parks a configuration they are not using yet.
func ValidateModelRouting(other map[string]any) error {
	raw, present := other["model_routing"]
	if !present || raw == nil {
		return nil
	}
	if _, ok := raw.(map[string]any); !ok {
		return errors.New("other_config.model_routing must be an object")
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("model_routing: %w", err)
	}

	var cfg ModelRoutingConfig
	// The typed unmarshal is the type check: a string in min_runes or a scalar in tiers fails here
	// instead of being quietly dropped at read time.
	if err := json.Unmarshal(body, &cfg); err != nil {
		return fmt.Errorf("model_routing: %w", err)
	}
	if !cfg.Enabled {
		return nil
	}
	if len(cfg.Tiers) == 0 {
		return errors.New("model_routing.enabled requires at least one tier")
	}
	if len(cfg.Tiers) > modelRoutingMaxTiers {
		return fmt.Errorf("model_routing.tiers: %d tiers exceeds the %d limit", len(cfg.Tiers), modelRoutingMaxTiers)
	}
	usable := 0
	for i, tier := range cfg.Tiers {
		if strings.TrimSpace(tier.Model) == "" {
			continue
		}
		usable++
		if tier.MinRunes < 0 || tier.MinTokens < 0 {
			return fmt.Errorf("model_routing.tiers[%d]: min_runes and min_tokens must be >= 0", i)
		}
	}
	if usable == 0 {
		return errors.New("model_routing.tiers: every tier is missing \"model\"")
	}
	if cfg.Classifier != nil && strings.TrimSpace(cfg.Classifier.Model) == "" {
		// The read path drops this and routes without it; the operator would keep paying for a
		// classifier that was never configured.
		return errors.New("model_routing.classifier.model is required when a classifier is set")
	}
	if cfg.Classifier != nil && (cfg.Classifier.TimeoutMs < 0 || cfg.Classifier.CacheTTLSeconds < 0) {
		return errors.New("model_routing.classifier: timeout_ms and cache_ttl_seconds must be >= 0")
	}
	return nil
}
