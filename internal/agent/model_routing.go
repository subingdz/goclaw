package agent

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Per-turn model routing: choose the model *before* the turn runs, from what the turn asks for.
//
// This is not `model_fallback` (which reacts to a failed call) and it is not the media tool
// chains (which pick a backend by capability). It decides, for a user-visible turn, that this
// request is big/code-shaped/media-heavy enough to deserve a different model than the agent's
// configured one. The mechanism it rides on is the existing `ModelOverride`/`ProviderOverride`
// pair on RunRequest — the same one cron and heartbeat already use — so the budget pre-estimate,
// the reasoning-effort resolution and the provider unwrapping downstream all see a coherent
// choice instead of a surprise mid-run swap.
//
// What routing deliberately does NOT do: pretend to know which model is stronger. Nothing a
// provider publishes says so. Tiers are an ordered list and the first matching one wins, which
// keeps "how do I rank these?" an operator decision written in config rather than a table we
// guess at and get wrong quietly.

// routingSignals is everything a rule can look at before the turn has done any work.
type routingSignals struct {
	Runes      int
	Tokens     int
	HasCode    bool
	MediaCount int
	RunKind    string
	// Message is the raw turn text, carried for the classifier (rules never read it).
	Message string
}

func newRoutingSignals(l *Loop, message string, media []bus.MediaFile, runKind string) routingSignals {
	s := routingSignals{
		Runes:      len([]rune(message)),
		MediaCount: len(media),
		// Labelled, not raw: a user-initiated run has an empty RunKind, and `run_kinds: [""]` is
		// not something an operator should have to write in the panel to mean "human turns".
		RunKind: runKindLabel(runKind),
		Message: message,
		HasCode: looksLikeCodeTask(message),
	}
	if l != nil && l.tokenCounter != nil {
		// Counted with the agent's configured tokenizer. The routed model may tokenize
		// differently, so MinTokens is an estimate threshold, not a billable figure — which is
		// why MinRunes exists as the tokenizer-free option.
		s.Tokens = l.tokenCounter.Count(l.model, message)
	}
	return s
}

// fencedCodeMarkers are the cheap, language-agnostic signs that a turn is asking for code work
// rather than mentioning a keyword in prose. Backticks come in pairs, so a single one is noise.
func looksLikeCodeTask(message string) bool {
	if strings.Contains(message, "```") {
		return true
	}
	lowered := strings.ToLower(message)
	for _, marker := range []string{"stack trace", "panic:", "traceback", "compilation error", "build failed", "test failed", "segmentation fault"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// tierMatches requires every *declared* condition to hold. Zero values mean "not a condition",
// so a tier with only a model matches every turn — which is how an operator writes
// "always use this model", and is why the list is ordered rather than scored.
func tierMatches(t store.ModelRoutingTier, s routingSignals) bool {
	if t.MinRunes > 0 && s.Runes < t.MinRunes {
		return false
	}
	if t.MinTokens > 0 && s.Tokens < t.MinTokens {
		return false
	}
	if t.RequiresCode && !s.HasCode {
		return false
	}
	if t.RequiresMedia && s.MediaCount == 0 {
		return false
	}
	if len(t.RunKinds) > 0 && !containsFolded(t.RunKinds, s.RunKind) {
		return false
	}
	return true
}

func containsFolded(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return true
		}
	}
	return false
}

// runKindLabel names the empty RunKind (user-initiated) so a tier can say "user" instead of "".
// Unknown kinds pass through, so a future kind does not silently stop matching.
func runKindLabel(runKind string) string {
	if strings.TrimSpace(runKind) == "" {
		return "user"
	}
	return runKind
}

// routingDecision is what a caller applies to a run, plus the audit trail. Reason is written for
// a human reading a trace: it names the tier and the condition that fired, so "why did this turn
// cost more" has an answer that is not a guess.
type routingDecision struct {
	Tier   string
	Reason string
	Source string // "rules", "classifier", or "" when nothing changed
	From   string
	To     string

	tier *store.ModelRoutingTier
}

func (d *routingDecision) applied() bool { return d != nil && d.To != "" && d.To != d.From }

// routingCounters answers "is this policy doing anything" for the human tuning it. They live on the
// Loop, hence per agent-cache-entry: the window starts when the agent is resolved and resets when
// agents.update drops the cached Loop (or the router TTL expires it). That is the point — a DB
// table would take one write per turn to answer a question that only matters for the next few
// minutes of tuning. Read-only through the WS method `agents.routingStats`.
type routingCounters struct {
	mu               sync.Mutex
	decisions        int64
	changes          int64
	noMatch          int64
	providerMisses   int64
	classifierCalls  int64
	classifierHits   int64
	classifierFailed int64
	byTier           map[string]int64
	// since is when this agent was resolved — the window the numbers describe.
	since time.Time
}

func newRoutingCounters() *routingCounters {
	return &routingCounters{byTier: make(map[string]int64), since: time.Now().UTC()}
}

// RoutingStats is the wire shape of those counters.
type RoutingStats struct {
	WindowStart         time.Time        `json:"windowStart"`
	AgentModel          string           `json:"agentModel"`
	Decisions           int64            `json:"decisions"`
	Changes             int64            `json:"changes"`
	NoMatch             int64            `json:"noMatch"`
	ProviderMisses      int64            `json:"providerMisses"`
	ClassifierCalls     int64            `json:"classifierCalls"`
	ClassifierCacheHits int64            `json:"classifierCacheHits"`
	ClassifierFailures  int64            `json:"classifierFailures"`
	ByTier              map[string]int64 `json:"byTier,omitempty"`
}

func (c *routingCounters) note(decisions, changes, noMatch, providerMiss int64, tier string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions += decisions
	c.changes += changes
	c.noMatch += noMatch
	c.providerMisses += providerMiss
	if changes > 0 {
		if c.byTier == nil {
			c.byTier = make(map[string]int64)
		}
		c.byTier[tier] += changes
	}
}

func (c *routingCounters) noteClassifier(calls, hits, failures bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if calls {
		c.classifierCalls++
	}
	if hits {
		c.classifierHits++
	}
	if failures {
		c.classifierFailed++
	}
}

// RoutingStats snapshots the live counters. Only *Loop* implements it on purpose: the router hands
// the snapshot out through an optional-interface check, so adding routing stats later to another
// Agent implementation would not force a method onto the Agent interface.
func (l *Loop) RoutingStats() RoutingStats {
	out := RoutingStats{AgentModel: l.model, ByTier: map[string]int64{}}
	if l.routingCounters == nil {
		return out
	}
	l.routingCounters.mu.Lock()
	defer l.routingCounters.mu.Unlock()
	out.WindowStart = l.routingCounters.since
	out.Decisions = l.routingCounters.decisions
	out.Changes = l.routingCounters.changes
	out.NoMatch = l.routingCounters.noMatch
	out.ProviderMisses = l.routingCounters.providerMisses
	out.ClassifierCalls = l.routingCounters.classifierCalls
	out.ClassifierCacheHits = l.routingCounters.classifierHits
	out.ClassifierFailures = l.routingCounters.classifierFailed
	for tier, n := range l.routingCounters.byTier {
		out.ByTier[tier] = n
	}
	return out
}

// routeModel decides the model for one run and returns what it chose. It never fails a run: an
// unresolvable provider, a classifier timeout, or a malformed bag all leave the agent's own
// model in place, because routing is an optimisation and a broken optimisation must be invisible.
//
// Observability is three things and no new data plane: one Info line per applied decision, a
// "model_route:<tier>" trace tag, and usage events, which already record the model that actually
// ran (usage_events.go sets event.Model = result.Model) so routed cost lands in the existing usage
// page. Aggregate counters live in memory and are read through `agents.routingStats`.
func (l *Loop) routeModel(ctx context.Context, req *RunRequest) *routingDecision {
	// An explicit caller override wins outright. Cron and heartbeat already name a model on
	// purpose, and the last thing they need is a per-agent policy second-guessing it.
	if req == nil || req.ModelOverride != "" {
		return nil
	}
	cfg := store.FindModelRoutingConfig(l.agentOtherConfig)
	if cfg == nil {
		return nil
	}

	signals := newRoutingSignals(l, req.Message, req.Media, req.RunKind)
	decision := &routingDecision{From: l.model, To: l.model}

	if tier, reason := firstMatchingTier(cfg.Tiers, signals); tier != nil {
		decision.tier, decision.Tier, decision.Reason, decision.Source = tier, tier.Name, reason, "rules"
	} else if cfg.Classifier != nil {
		// classifyTier counts its own outcomes (call / cache hit / failure) — it is the only place
		// that knows which of the three happened.
		if tier, reason, _ := l.classifyTier(ctx, cfg, signals); tier != nil {
			decision.tier, decision.Tier, decision.Reason, decision.Source = tier, tier.Name, reason, "classifier"
		}
	}
	if decision.tier == nil {
		l.routingCounters.note(1, 0, 1, 0, "")
		return decision
	}

	provider, ok := l.resolveRoutingProvider(ctx, decision.tier.Provider)
	if !ok {
		slog.Warn("model_routing: provider unavailable, keeping the agent's model",
			"agent", l.id, "tier", decision.Tier, "provider", decision.tier.Provider, "model", l.model)
		l.routingCounters.note(1, 0, 1, 1, "")
		return decision
	}

	if decision.tier.Model != l.model {
		req.ModelOverride = decision.tier.Model
		if provider != l.provider {
			req.ProviderOverride = provider
		}
		decision.To = decision.tier.Model
	}
	// A tier that matches and changes nothing is still recorded: "the policy fires but routes to
	// the model the agent already has" is the most common reason a routing card does nothing, and
	// without this it would look identical to "no turn ever matched".
	if decision.applied() {
		l.routingCounters.note(1, 1, 0, 0, decision.Tier)
	} else {
		l.routingCounters.note(1, 0, 1, 0, "")
	}
	if decision.applied() {
		// TraceTags are how a routing decision shows up in the trace UI without a schema change.
		req.TraceTags = append(req.TraceTags, "model_route:"+decision.Tier)
		slog.Info("model_routing: decision",
			"agent", l.id, "session", req.SessionKey, "tier", decision.Tier,
			"from", decision.From, "to", decision.To, "source", decision.Source, "reason", decision.Reason,
			"runes", signals.Runes, "tokens", signals.Tokens, "media", signals.MediaCount, "run_kind", signals.RunKind)
	}
	return decision
}

// firstMatchingTier walks the ordered list; no match means "use the agent's model".
func firstMatchingTier(tiers []store.ModelRoutingTier, s routingSignals) (*store.ModelRoutingTier, string) {
	for i := range tiers {
		t := &tiers[i]
		if !tierMatches(*t, s) {
			continue
		}
		return t, describeMatch(*t, s)
	}
	return nil, ""
}

func describeMatch(t store.ModelRoutingTier, s routingSignals) string {
	parts := make([]string, 0, 4)
	if t.MinRunes > 0 {
		parts = append(parts, "runes>="+strconv.Itoa(t.MinRunes))
	}
	if t.MinTokens > 0 {
		parts = append(parts, "tokens>="+strconv.Itoa(t.MinTokens))
	}
	if t.RequiresCode {
		parts = append(parts, "code")
	}
	if t.RequiresMedia {
		parts = append(parts, "media")
	}
	if len(t.RunKinds) > 0 {
		parts = append(parts, "kind="+strings.Join(t.RunKinds, "|"))
	}
	if len(parts) == 0 {
		return "unconditional tier"
	}
	return strings.Join(parts, ",")
}

// resolveRoutingProvider maps a tier's provider name to a live provider. Empty means "the
// agent's own", and a name that cannot be resolved degrades to that instead of failing the turn.
// Lookup is tenant-scoped (tenant-specific first, master fallback) exactly like the agent's own
// provider was resolved, so a routing policy in one tenant's agent config cannot reach another
// tenant's credentials.
func (l *Loop) resolveRoutingProvider(_ context.Context, name string) (providers.Provider, bool) {
	if name == "" || l.providerReg == nil {
		return l.provider, true
	}
	p, err := l.providerReg.GetForTenant(l.tenantID, name)
	if err != nil || p == nil {
		return l.provider, false
	}
	return p, true
}
