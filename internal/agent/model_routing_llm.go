package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	usagecaps "github.com/nextlevelbuilder/goclaw/internal/usage/caps"
)

// The classifier is the optional second half of routing: rules answer first, and only when no
// rule matched does a cheap model get asked "which of these tiers is this turn?".
//
// Two properties hold this design together:
//
//  1. It is strictly advisory. Every failure path — timeout, unparseable answer, a name that is
//     not one of the configured tiers, a provider that cannot be resolved, a usage-cap refusal —
//     returns nil and the turn runs on the agent's own model. A router that can break a
//     conversation is worse than no router.
//  2. It is bounded. One call, one short deadline, a tiny output budget, thinking disabled, and
//     the same usage-cap preflight the agent loop uses, so the classification is billed and
//     capped like any other call instead of being free money spent invisibly. Being agent-scoped
//     is deliberate: the guard then reserves the agent's own max output against its own context
//     window, so a classifier can never be asked of a deployment whose main call would not fit.
//
// What it deliberately is NOT: a difficulty score, a benchmark table, or an opinion about which
// vendor is stronger. The classifying model picks among *the operator's own tiers*, described by
// the operator's own `hint` text. We add no knowledge; we only ask a cheap model to read the
// request and choose.

// classifierOutputBudget is small on purpose: the answer is one tier name. Reasoning models
// spend output tokens on thinking, which is why OptThinkingLevel is pinned off below (the same
// trap title generation hit).
const classifierOutputBudget = 24

// classifierMessagePreviewChars caps how much of the turn the classifier reads. A tier decision
// needs the shape of a request, not its payload — and a 40k-character paste would make the
// "cheap" call the most expensive part of the turn.
const classifierMessagePreviewChars = 1200

// routingClassifierCache memoises answers per Loop (hence per cached agent). Re-sending the same
// message within a few minutes — a retried turn, an edited-and-resent prompt, a duplicate webhook
// delivery — must not pay for the classifier twice.
type routingClassifierCache struct {
	mu      sync.Mutex
	entries map[string]routingClassifierEntry
	// blockedUntil is the failure cooldown deadline (zero when not paused).
	blockedUntil time.Time
}

type routingClassifierEntry struct {
	tierName  string
	expiresAt time.Time
}

// routingClassifierCacheCap bounds memory for a long-running Loop. Eviction is a clear rather
// than an LRU: the cache exists to absorb bursts, and dropping it costs one extra cheap call per
// message, never correctness.
const routingClassifierCacheCap = 256

// classifierFailureCooldown pauses the classifier after a call fails, instead of caching the
// failure as an answer. Two wrongs were avoided here: caching "baseline" for the full TTL would
// silently suppress routing for ten minutes after one blip, and not pausing at all would let a
// permanently misconfigured classifier add timeout_ms to every unrouted turn forever.
const classifierFailureCooldown = time.Minute

func newRoutingClassifierCache() *routingClassifierCache {
	return &routingClassifierCache{entries: make(map[string]routingClassifierEntry)}
}

// classifyTier asks the configured classifier model which tier this turn belongs to.
// Returns (tier, reason, calledLLM). tier==nil means "route nothing", and calledLLM reports
// whether a call was actually paid for, so a cache hit or a failure-pause is never billed to the
// model in a test or a log reading.
func (l *Loop) classifyTier(ctx context.Context, cfg *store.ModelRoutingConfig, signals routingSignals) (*store.ModelRoutingTier, string, bool) {
	cl := cfg.Classifier
	if cl == nil || len(cfg.Tiers) == 0 {
		return nil, "", false
	}

	key := classifierCacheKey(signals, cfg)
	if l.routingCache.isBlocked() {
		return nil, "classifier paused after a failed call", false
	}
	if tier, ok := l.routingCache.get(key, cfg.Tiers); ok {
		l.routingCounters.noteClassifier(false, true, false)
		if tier == nil {
			return nil, "classifier: baseline (cached)", false
		}
		return tier, "classifier: cached answer", false
	}

	provider, ok := l.resolveRoutingProvider(ctx, cl.Provider)
	if !ok {
		slog.Debug("model_routing: classifier provider unavailable, keeping the agent's model",
			"agent", l.id, "provider", cl.Provider)
		return nil, "", false
	}

	timeout := time.Duration(cl.TimeoutMs) * time.Millisecond
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := l.usageCaps.Chat(callCtx, provider, providers.ChatRequest{
		Model:    cl.Model,
		Messages: classifierMessages(cfg.Tiers, signals),
		Options: map[string]any{
			providers.OptMaxTokens:     classifierOutputBudget,
			providers.OptTemperature:   0,
			providers.OptThinkingLevel: "off",
		},
	}, usagecaps.ChatOptions{
		TenantID:           l.tenantID,
		AgentID:            l.agentUUID,
		ProviderName:       provider.Name(),
		ModelID:            cl.Model,
		Purpose:            "model-routing",
		MaxOutputTokens:    classifierOutputBudget,
		AgentContextWindow: l.contextWindow,
		AgentMaxTokens:     l.effectiveMaxTokens(),
	})
	if err != nil {
		// Debug, not Warn: this fires on every turn when a board has no key for the classifier
		// provider, and routing is an optimisation whose failure must stay quiet.
		slog.Debug("model_routing: classifier call failed, keeping the agent's model",
			"agent", l.id, "model", cl.Model, "timeout_ms", cl.TimeoutMs, "error", err)
		l.routingCounters.noteClassifier(true, false, true)
		l.routingCache.blockFor(classifierFailureCooldown)
		return nil, "", true
	}
	if resp == nil {
		slog.Debug("model_routing: classifier returned no response, keeping the agent's model",
			"agent", l.id, "model", cl.Model)
		l.routingCounters.noteClassifier(true, false, true)
		l.routingCache.blockFor(classifierFailureCooldown)
		return nil, "", true
	}

	tier, answer := matchClassifierAnswer(resp.Content, cfg.Tiers)
	l.routingCounters.noteClassifier(true, false, false)

	// Cache the *outcome*, not the raw answer: "baseline", an unrecognisable reply and a tier that
	// is not configured all mean "route nothing", and must read back as a cached no-op instead of
	// being re-asked every turn. Storing the model's own word ("baseline") would miss on lookup.
	cached := ""
	if tier != nil {
		cached = tier.Name
	}
	l.routingCache.put(key, cached, time.Duration(cl.CacheTTLSeconds)*time.Second)
	if tier == nil {
		// The reason string is what reaches the audit log when routing still changes nothing, so
		// naming an unknown tier and answering "baseline" are recorded differently even though
		// both keep the agent's own model — "the model ignored my policy" and "the model read my
		// policy and agreed" are different operational problems.
		if answer == "" {
			slog.Debug("model_routing: classifier answer named no configured tier",
				"agent", l.id, "answer", truncateStr(strings.TrimSpace(resp.Content), 80), "model", cl.Model)
			return nil, "classifier: unrecognised answer", true
		}
		return nil, "classifier: " + answer, true
	}
	return tier, "classifier: " + answer, true
}

// classifierCacheKey folds the tier set into the key: editing the policy (or a hint the model
// reads) must not keep serving yesterday's answer for the same message.
func classifierCacheKey(s routingSignals, cfg *store.ModelRoutingConfig) string {
	h := sha256.New()
	h.Write([]byte(s.RunKind))
	h.Write([]byte{0})
	h.Write([]byte(s.Message))
	for _, t := range cfg.Tiers {
		h.Write([]byte{0})
		h.Write([]byte(t.Name))
		h.Write([]byte{0})
		h.Write([]byte(t.Hint))
	}
	return hex.EncodeToString(h.Sum(nil))
}

const classifierSystemPrompt = `You route one user turn to one model tier. Pick the single tier whose description best fits what the user is asking for. Use BASELINE when no tier fits, the request is short or conversational, or you are unsure. Reply with only the tier name on one line.`

func classifierMessages(tiers []store.ModelRoutingTier, signals routingSignals) []providers.Message {
	var b strings.Builder
	b.Grow(256 + len(tiers)*64)
	b.WriteString("BASELINE: the agent's own configured model. Use it unless a tier clearly fits.\n")
	for _, t := range tiers {
		b.WriteString("- ")
		b.WriteString(t.Name)
		b.WriteString(": ")
		if hint := strings.TrimSpace(t.Hint); hint != "" {
			b.WriteString(hint)
		} else {
			b.WriteString("model ")
			b.WriteString(t.Model)
		}
		b.WriteByte('\n')
	}

	var meta strings.Builder
	meta.WriteString("Turn: ")
	meta.WriteString(fmt.Sprintf("%d characters", signals.Runes))
	if signals.MediaCount > 0 {
		meta.WriteString(fmt.Sprintf(", %d attachment(s)", signals.MediaCount))
	}
	if signals.HasCode {
		meta.WriteString(", contains code")
	}
	if signals.RunKind != "" {
		meta.WriteString(", run kind " + signals.RunKind)
	}
	meta.WriteString(".\n\n")

	return []providers.Message{
		{Role: "system", Content: classifierSystemPrompt},
		{Role: "user", Content: meta.String() + truncateStr(signals.Message, classifierMessagePreviewChars)},
	}
}

// matchClassifierAnswer maps a free-form reply to a tier. Tolerant by necessity: models add
// markdown, quotes and "TIER: name" prefixes. Anything unrecognisable is a baseline, and the
// empty answer is checked first so a chatty reply cannot match by substring.
func matchClassifierAnswer(content string, tiers []store.ModelRoutingTier) (*store.ModelRoutingTier, string) {
	answer := strings.TrimSpace(strings.Trim(strings.TrimSpace(content), "`\"'* "))
	if answer == "" {
		return nil, ""
	}
	lowered := strings.ToLower(answer)
	if lowered == "baseline" || lowered == "-" || lowered == "none" {
		return nil, "baseline"
	}
	// Exact name first, then the first tier name that appears inside the reply.
	for i := range tiers {
		if strings.ToLower(tiers[i].Name) == lowered {
			return &tiers[i], tiers[i].Name
		}
	}
	for i := range tiers {
		name := strings.ToLower(tiers[i].Name)
		if name != "" && strings.Contains(lowered, name) {
			return &tiers[i], tiers[i].Name
		}
	}
	return nil, ""
}

// get returns a cached decision. A cached baseline is real information (the model was asked and
// said "nothing fits"), so it is returned as (nil, true) rather than re-asking.
func (c *routingClassifierCache) get(key string, tiers []store.ModelRoutingTier) (*store.ModelRoutingTier, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	if entry.tierName == "" {
		return nil, true // cached baseline
	}
	for i := range tiers {
		if tiers[i].Name == entry.tierName {
			return &tiers[i], true
		}
	}
	return nil, false
}

func (c *routingClassifierCache) put(key, tierName string, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]routingClassifierEntry)
	}
	if len(c.entries) >= routingClassifierCacheCap {
		clear(c.entries)
	}
	c.entries[key] = routingClassifierEntry{tierName: tierName, expiresAt: time.Now().Add(ttl)}
}

// isBlocked reports whether the classifier is in its post-failure pause.
func (c *routingClassifierCache) isBlocked() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockedUntil.IsZero() {
		return false
	}
	if time.Now().After(c.blockedUntil) {
		c.blockedUntil = time.Time{}
		return false
	}
	return true
}

func (c *routingClassifierCache) blockFor(d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockedUntil = time.Now().Add(d)
}
