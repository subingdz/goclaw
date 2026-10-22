package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// routingStub records every request so a test can assert both the answer a routing path saw and
// how many paid calls it made.
type routingStub struct {
	name     string
	response string
	err      error
	calls    int
	seen     []providers.ChatRequest
}

func (s *routingStub) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	s.calls++
	s.seen = append(s.seen, req)
	if s.err != nil {
		return nil, s.err
	}
	return &providers.ChatResponse{Content: s.response}, nil
}

func (s *routingStub) ChatStream(_ context.Context, req providers.ChatRequest, _ func(providers.StreamChunk)) (*providers.ChatResponse, error) {
	return s.Chat(context.Background(), req)
}

func (s *routingStub) DefaultModel() string { return "stub-default" }
func (s *routingStub) Name() string         { return s.name }

// routingLoop builds a Loop through NewLoop so the classifier-cache wiring is itself exercised
// (a hand-made struct would route differently from production and prove nothing).
func routingLoop(t *testing.T, bag string, agentProvider providers.Provider, reg *providers.Registry, tenant uuid.UUID) *Loop {
	t.Helper()
	if agentProvider == nil {
		agentProvider = &routingStub{name: "own", response: "baseline"}
	}
	return NewLoop(LoopConfig{
		ID:               "router-test",
		AgentUUID:        uuid.New(),
		TenantID:         tenant,
		AgentOtherConfig: json.RawMessage(bag),
		Provider:         agentProvider,
		Model:            "agent-model",
		// A realistic window: the classifier call is agent-scoped, so caps reserves the agent's
		// own max output (8192 by default) against this, exactly as the main call does. An 8k
		// fixture makes every classifier call abort — that is the agent's config, not the router.
		ContextWindow:   32000,
		ProviderReg:     reg,
		InjectionAction: "off",
		MaxIterations:   1,
	})
}

func tierBag(tiers ...string) string {
	return `{"model_routing":{"enabled":true,"tiers":[` + strings.Join(tiers, ",") + `]}}`
}

func TestRouteModel_CallerOverrideWins(t *testing.T) {
	l := routingLoop(t, tierBag(`{"model":"routed","min_runes":1}`), nil, nil, providers.MasterTenantID)
	req := &RunRequest{Message: "hi", ModelOverride: "heartbeat-cheap"}

	if d := l.routeModel(context.Background(), req); d != nil {
		t.Fatalf("expected no decision when the caller already named a model, got %+v", d)
	}
	if req.ModelOverride != "heartbeat-cheap" {
		t.Errorf("caller override mutated to %q", req.ModelOverride)
	}
}

func TestRouteModel_NoConfigIsNoop(t *testing.T) {
	cases := map[string]string{
		"absent":   `{"prompt_mode":"full"}`,
		"disabled": `{"model_routing":{"enabled":false,"tiers":[{"model":"x","min_runes":1}]}}`,
		"broken":   `{"model_routing":{"enabled":true,"tiers":[{"hint":"no model"}]}}`,
	}
	for name, bag := range cases {
		t.Run(name, func(t *testing.T) {
			l := routingLoop(t, bag, nil, nil, providers.MasterTenantID)
			req := &RunRequest{Message: strings.Repeat("长", 500)}
			if d := l.routeModel(context.Background(), req); d != nil {
				t.Fatalf("expected nil decision for %s config", name)
			}
			if req.ModelOverride != "" || req.ProviderOverride != nil {
				t.Errorf("routing changed the request without a policy: %+v", req)
			}
		})
	}
}

func TestRouteModel_RuleSetsModelOnOwnProvider(t *testing.T) {
	l := routingLoop(t, tierBag(`{"name":"heavy","model":"routed","min_runes":100}`), nil, nil, providers.MasterTenantID)
	req := &RunRequest{Message: strings.Repeat("a", 120), SessionKey: "s1"}

	d := l.routeModel(context.Background(), req)
	if d == nil || !d.applied() {
		t.Fatalf("expected an applied decision, got %+v", d)
	}
	if req.ModelOverride != "routed" {
		t.Errorf("ModelOverride = %q, want routed", req.ModelOverride)
	}
	// The tier named no provider, so the agent's own must stay in place: setting ProviderOverride
	// here would be a different provider object than l.provider and change auth silently.
	if req.ProviderOverride != nil {
		t.Errorf("ProviderOverride should stay nil, got %s", req.ProviderOverride.Name())
	}
	if !containsFolded(req.TraceTags, "model_route:heavy") {
		t.Errorf("trace tags = %v, want the tier tag", req.TraceTags)
	}
	if d.Source != "rules" || d.From != "agent-model" || d.To != "routed" {
		t.Errorf("decision = %+v", d)
	}
	if !strings.Contains(d.Reason, "runes>=100") {
		t.Errorf("reason = %q, want it to name the threshold that fired", d.Reason)
	}
}

func TestRouteModel_FirstMatchWins(t *testing.T) {
	// Both tiers match this turn. Ordering, not scoring, is the policy — this pins that.
	l := routingLoop(t, tierBag(
		`{"name":"first","model":"m-first","min_runes":1}`,
		`{"name":"second","model":"m-second","min_runes":1}`,
	), nil, nil, providers.MasterTenantID)
	req := &RunRequest{Message: "hello there"}

	d := l.routeModel(context.Background(), req)
	if d.Tier != "first" || req.ModelOverride != "m-first" {
		t.Fatalf("got tier=%q model=%q, want first/m-first", d.Tier, req.ModelOverride)
	}
}

func TestRouteModel_DeclaredConditionsAreAND(t *testing.T) {
	l := routingLoop(t, tierBag(`{"name":"code","model":"m-code","min_runes":10,"requires_code":true}`), nil, nil, providers.MasterTenantID)

	long := &RunRequest{Message: "please refactor the module and explain"}
	if d := l.routeModel(context.Background(), long); d != nil && d.applied() {
		t.Fatalf("long prose must not match a requires_code tier, got %+v", d)
	}

	code := &RunRequest{Message: "fix this please:\n```go\nfunc main() {}\n```"}
	if d := l.routeModel(context.Background(), code); d == nil || !d.applied() {
		t.Fatalf("short code block must match, got %+v", d)
	}
}

func TestRouteModel_ZeroThresholdTierMatchesEverything(t *testing.T) {
	// This is how an operator writes "always use this model" — a tier with only a model.
	l := routingLoop(t, tierBag(`{"name":"always","model":"m-always"}`), nil, nil, providers.MasterTenantID)
	req := &RunRequest{Message: "ok"}
	if d := l.routeModel(context.Background(), req); d == nil || !d.applied() {
		t.Fatalf("unconditional tier should always route, got %+v", d)
	}
}

func TestRouteModel_MediaAndRunKindSignals(t *testing.T) {
	l := routingLoop(t, tierBag(
		`{"name":"vision","model":"m-vision","requires_media":true}`,
		`{"name":"deleg","model":"m-deleg","run_kinds":["delegation"]}`,
		`{"name":"human","model":"m-human","run_kinds":["user"]}`,
	), nil, nil, providers.MasterTenantID)

	media := &RunRequest{Message: "what is this", Media: []bus.MediaFile{{Path: "/tmp/a.png", MimeType: "image/png"}}}
	if d := l.routeModel(context.Background(), media); d == nil || d.Tier != "vision" {
		t.Fatalf("attachment turn got %+v, want vision", d)
	}

	deleg := &RunRequest{Message: "do the work", RunKind: "delegation"}
	if d := l.routeModel(context.Background(), deleg); d == nil || d.Tier != "deleg" {
		t.Fatalf("delegation turn got %+v, want deleg", d)
	}

	// An empty RunKind is a human turn, and the panel writes "user" — not "".
	human := &RunRequest{Message: "hello there, how is it going"}
	if d := l.routeModel(context.Background(), human); d == nil || d.Tier != "human" {
		t.Fatalf("user turn got %+v, want human", d)
	}
	if s := newRoutingSignals(l, "x", nil, ""); s.RunKind != "user" {
		t.Errorf("run kind label = %q, want user", s.RunKind)
	}
	if s := newRoutingSignals(l, "x", nil, "future_kind"); s.RunKind != "future_kind" {
		t.Errorf("unknown kinds must pass through, got %q", s.RunKind)
	}
}

func TestRouteModel_UnresolvableProviderKeepsAgentModel(t *testing.T) {
	tenant := uuid.New()
	reg := providers.NewRegistry(func(context.Context) uuid.UUID { return tenant })
	reg.RegisterForTenant(tenant, &routingStub{name: "own"})

	l := routingLoop(t, tierBag(`{"name":"elsewhere","model":"m-x","provider":"not-registered","min_runes":1}`),
		&routingStub{name: "own"}, reg, tenant)
	req := &RunRequest{Message: "hello"}

	d := l.routeModel(context.Background(), req)
	if d.applied() {
		t.Fatalf("a tier whose provider is missing must not change the model, got %+v", d)
	}
	if req.ModelOverride != "" || req.ProviderOverride != nil {
		t.Errorf("request mutated despite an unresolvable provider: %+v", req)
	}
}

func TestRouteModel_CrossProviderSetsProviderOverride(t *testing.T) {
	tenant := uuid.New()
	own := &routingStub{name: "own"}
	other := &routingStub{name: "silicon"}
	reg := providers.NewRegistry(func(context.Context) uuid.UUID { return tenant })
	reg.RegisterForTenant(tenant, own)
	reg.RegisterForTenant(tenant, other)

	l := routingLoop(t, tierBag(`{"name":"cheap","model":"m-cheap","provider":"silicon","min_runes":1}`), own, reg, tenant)
	req := &RunRequest{Message: "hello"}

	if d := l.routeModel(context.Background(), req); !d.applied() {
		t.Fatalf("expected applied decision, got %+v", d)
	}
	if req.ProviderOverride != providers.Provider(other) {
		t.Fatalf("ProviderOverride should be the tier's provider")
	}
	if req.ProviderOverride.Name() != "silicon" {
		t.Errorf("provider = %q", req.ProviderOverride.Name())
	}
}

// A routing policy is per-agent config, so it must not be able to reach another tenant's
// credentials through the master fallback used for ordinary provider resolution.
func TestRouteModel_TenantIsolation(t *testing.T) {
	otherTenant := uuid.New()
	reg := providers.NewRegistry(func(context.Context) uuid.UUID { return otherTenant })
	reg.RegisterForTenant(otherTenant, &routingStub{name: "private"})

	l := routingLoop(t, tierBag(`{"name":"steal","model":"m","provider":"private","min_runes":1}`),
		&routingStub{name: "own"}, reg, providers.MasterTenantID)
	req := &RunRequest{Message: "hello"}

	if d := l.routeModel(context.Background(), req); d.applied() {
		t.Fatalf("master-scope agent must not resolve another tenant's provider, got %+v", d)
	}
}

func TestLooksLikeCodeTask(t *testing.T) {
	cases := map[string]bool{
		"write a haiku about rain":                 false,
		"an inline `pkg` reference in prose":       false,
		"```go\nfunc main() {}\n```":               true,
		"runtime error: panic: index out of range": true,
		"Traceback (most recent call last)":        true,
		"the build failed on arm64":                true,
		"I compiled my feelings about python":      false,
	}
	for msg, want := range cases {
		if got := looksLikeCodeTask(msg); got != want {
			t.Errorf("looksLikeCodeTask(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestMatchClassifierAnswer(t *testing.T) {
	tiers := []store.ModelRoutingTier{{Name: "strong", Model: "m1"}, {Name: "cheap", Model: "m2"}}
	cases := []struct {
		answer string
		want   string // "" means no routing
	}{
		{answer: "strong", want: "strong"},
		{answer: "  STRONG  ", want: "strong"},
		{answer: "**strong**", want: "strong"},
		{answer: "Tier: strong", want: "strong"},
		{answer: "baseline", want: ""},
		{answer: "-", want: ""},
		{answer: "", want: ""},
		{answer: "I cannot determine", want: ""},
		{answer: "cheap", want: "cheap"},
	}
	for _, tc := range cases {
		tier, _ := matchClassifierAnswer(tc.answer, tiers)
		if tc.want == "" {
			if tier != nil {
				t.Errorf("answer %q matched %q, want no routing", tc.answer, tier.Name)
			}
			continue
		}
		if tier == nil || tier.Name != tc.want {
			t.Errorf("answer %q → %+v, want %q", tc.answer, tier, tc.want)
		}
	}
}

func TestRouteModel_ClassifierOnlyRunsWhenRulesMiss(t *testing.T) {
	classifier := &routingStub{name: "own", response: "strong"}
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"rule","model":"m-rule","min_runes":1},{"name":"strong","model":"m-strong"}],
		"classifier":{"model":"mini","timeout_ms":900}}}`

	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)
	req := &RunRequest{Message: "anything at all"}
	if d := l.routeModel(context.Background(), req); d == nil || d.Tier != "rule" || d.Source != "rules" {
		t.Fatalf("rules must answer first, got %+v", d)
	}
	if classifier.calls != 0 {
		t.Fatalf("classifier was paid for %d time(s) even though a rule matched", classifier.calls)
	}
}

func TestRouteModel_ClassifierPicksWhenNoRuleMatches(t *testing.T) {
	classifier := &routingStub{name: "own", response: "strong"}
	// min_runes:100000 makes every rule miss, which is the only way this test reaches the
	// classifier — a tier with no conditions matches everything and rules answer first.
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"strong","model":"m-strong","min_runes":100000,"hint":"multi-file refactors"},{"name":"cheap","model":"m-cheap","min_runes":100000,"hint":"greetings"}],
		"classifier":{"model":"mini","timeout_ms":900}}}`

	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)
	req := &RunRequest{Message: "refactor the auth module across three files"}
	d := l.routeModel(context.Background(), req)
	if d == nil || !d.applied() || d.Source != "classifier" || d.Tier != "strong" {
		t.Fatalf("got %+v, want classifier-selected strong", d)
	}
	if classifier.calls != 1 {
		t.Fatalf("calls = %d, want 1", classifier.calls)
	}
	// The paid call must be tiny and must not think: an unbounded classifier would cost more than
	// the turn it is routing.
	sent := classifier.seen[0]
	if sent.Model != "mini" {
		t.Errorf("classifier model = %q, want mini", sent.Model)
	}
	if got := sent.Options[providers.OptThinkingLevel]; got != "off" {
		t.Errorf("thinking level = %v, want off", got)
	}
	if got := sent.Options[providers.OptMaxTokens]; got != classifierOutputBudget {
		t.Errorf("max tokens = %v, want %d", got, classifierOutputBudget)
	}
}

func TestRouteModel_ClassifierAnswerIsCached(t *testing.T) {
	classifier := &routingStub{name: "own", response: "strong"}
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"strong","model":"m-strong","min_runes":100000}],
		"classifier":{"model":"mini","timeout_ms":900,"cache_ttl_seconds":60}}}`

	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)
	if l.routingCache == nil {
		t.Fatal("NewLoop must wire the classifier cache when a classifier is configured")
	}
	first := &RunRequest{Message: "same message"}
	if d := l.routeModel(context.Background(), first); d == nil || !d.applied() {
		t.Fatalf("first turn got %+v", d)
	}
	second := &RunRequest{Message: "same message"}
	cached := l.routeModel(context.Background(), second)
	if cached == nil || !cached.applied() {
		t.Fatalf("cached turn got %+v", cached)
	}
	if classifier.calls != 1 {
		t.Fatalf("calls = %d, want 1 (the repeat must be served from cache)", classifier.calls)
	}
	if !strings.Contains(cached.Reason, "cached") {
		t.Errorf("reason = %q, want it to say the answer was cached", cached.Reason)
	}
}

func TestRouteModel_BaselineAnswerIsCachedToo(t *testing.T) {
	classifier := &routingStub{name: "own", response: "BASELINE"}
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"strong","model":"m-strong","min_runes":100000}],
		"classifier":{"model":"mini","timeout_ms":900,"cache_ttl_seconds":60}}}`

	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)
	for i := 0; i < 2; i++ {
		req := &RunRequest{Message: "how are you"}
		if d := l.routeModel(context.Background(), req); d != nil && d.applied() {
			t.Fatalf("baseline must keep the agent's model, got %+v", d)
		}
	}
	if classifier.calls != 1 {
		t.Fatalf("calls = %d, want 1 — a cached baseline is still an answer", classifier.calls)
	}
}

// A classifier that fails must degrade to "no routing" and then stop being paid for, rather than
// adding timeout_ms to every turn of a permanently broken policy.
func TestRouteModel_ClassifierFailurePausesAndNeverBreaksRun(t *testing.T) {
	classifier := &routingStub{name: "own", err: errors.New("429 rate limited")}
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"strong","model":"m-strong","min_runes":100000}],
		"classifier":{"model":"mini","timeout_ms":900}}}`

	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)
	for i := 0; i < 3; i++ {
		req := &RunRequest{Message: "hello"}
		if d := l.routeModel(context.Background(), req); d != nil && d.applied() {
			t.Fatalf("failed classifier must not change the model, got %+v", d)
		}
	}
	if classifier.calls != 1 {
		t.Fatalf("calls = %d, want 1 — the failure cooldown should absorb the rest", classifier.calls)
	}

	// The pause is a pause, not a policy change: once it lapses, the next turn tries again.
	l.routingCache.blockedUntil = time.Time{}
	req := &RunRequest{Message: "hello"}
	l.routeModel(context.Background(), req)
	if classifier.calls != 2 {
		t.Fatalf("calls = %d, want 2 after the cooldown expires", classifier.calls)
	}
}

// The seam routing rides on. These four cases are what make a decision reach the wire — and the
// unwrap case is why a routed turn leaves the failure-driven fallback chain behind.
func TestEffectiveRunModelProvider(t *testing.T) {
	primary := &routingStub{name: "own"}
	wrapped := providers.NewModelFallbackProvider(
		providers.FallbackCandidate{ProviderName: "own", Model: "agent-model", Provider: primary},
		[]providers.FallbackCandidate{{ProviderName: "backup", Model: "m-backup", Provider: &routingStub{name: "backup"}}},
		0, false,
	)
	other := &routingStub{name: "silicon"}

	l := &Loop{model: "agent-model", provider: wrapped}

	if model, provider := l.effectiveRunModelProvider(&RunRequest{}); model != "agent-model" || provider != providers.Provider(wrapped) {
		t.Errorf("no override → (%q, %v), want the agent's own wrapped provider", model, provider.Name())
	}

	if model, provider := l.effectiveRunModelProvider(&RunRequest{ModelOverride: "m-routed"}); model != "m-routed" || provider != providers.Provider(primary) {
		t.Errorf("ModelOverride → (%q, %v), want m-routed on the unwrapped primary", model, provider.Name())
	}

	l2 := &Loop{model: "agent-model", provider: primary}
	if model, provider := l2.effectiveRunModelProvider(&RunRequest{ModelOverride: "m-routed"}); model != "m-routed" || provider != providers.Provider(primary) {
		t.Errorf("unwrapped provider must survive a ModelOverride, got (%q, %v)", model, provider.Name())
	}

	if model, provider := l.effectiveRunModelProvider(&RunRequest{ModelOverride: "m-routed", ProviderOverride: other}); model != "m-routed" || provider != providers.Provider(other) {
		t.Errorf("ProviderOverride must win, got (%q, %v)", model, provider.Name())
	}
}

func TestRouteModel_DecisionReachesTheEffectiveProvider(t *testing.T) {
	// End-to-end at the only seam that matters: routing writes the override pair, and the pair is
	// what the pipeline is built from. Together they prove a tier changes the call, not just a log.
	l := routingLoop(t, tierBag(`{"name":"heavy","model":"m-heavy","min_runes":50}`), nil, nil, providers.MasterTenantID)
	req := RunRequest{Message: strings.Repeat("work ", 20)}

	l.routeModel(context.Background(), &req)
	model, provider := l.effectiveRunModelProvider(&req)
	if model != "m-heavy" {
		t.Fatalf("effective model = %q, want m-heavy", model)
	}
	if provider != l.provider {
		t.Fatalf("effective provider changed without a tier naming one: %s", provider.Name())
	}
}

// The counters behind agents.routingStats. What matters here is that they distinguish the three
// things an operator cannot tell apart from the outside: nothing matched, matched-but-no-change, and
// an actual route.
func TestRoutingCounters(t *testing.T) {
	l := routingLoop(t, tierBag(
		`{"name":"heavy","model":"m-heavy","min_runes":100}`,
		`{"name":"same","model":"agent-model","requires_code":true}`,
	), nil, nil, providers.MasterTenantID)
	if l.routingCounters == nil {
		t.Fatal("an agent with a policy must carry counters")
	}

	l.routeModel(context.Background(), &RunRequest{Message: strings.Repeat("a", 150)})
	l.routeModel(context.Background(), &RunRequest{Message: "fix ```go\nx\n```"})
	l.routeModel(context.Background(), &RunRequest{Message: "hello"})

	stats := l.RoutingStats()
	if stats.Decisions != 3 {
		t.Errorf("decisions = %d, want 3", stats.Decisions)
	}
	if stats.Changes != 1 {
		t.Errorf("changes = %d, want 1", stats.Changes)
	}
	// noMatch counts both "nothing matched" and "matched but routed to the model already in use" —
	// the provider-miss case is separated out below so the two causes stay distinguishable.
	if stats.NoMatch != 2 {
		t.Errorf("noMatch = %d, want 2", stats.NoMatch)
	}
	if stats.ByTier["heavy"] != 1 {
		t.Errorf("byTier = %+v, want heavy:1", stats.ByTier)
	}
	if stats.WindowStart.IsZero() {
		t.Error("window start must be recorded at resolve time")
	}
	if stats.AgentModel != "agent-model" {
		t.Errorf("agentModel = %q", stats.AgentModel)
	}
}

func TestRoutingCountersProviderMiss(t *testing.T) {
	tenant := uuid.New()
	reg := providers.NewRegistry(func(context.Context) uuid.UUID { return tenant })
	reg.RegisterForTenant(tenant, &routingStub{name: "own"})
	l := routingLoop(t, tierBag(`{"name":"ghost","model":"m","provider":"absent","min_runes":1}`),
		&routingStub{name: "own"}, reg, tenant)

	l.routeModel(context.Background(), &RunRequest{Message: "hello"})

	stats := l.RoutingStats()
	if stats.ProviderMisses != 1 || stats.Changes != 0 {
		t.Errorf("got misses=%d changes=%d, want 1 and 0", stats.ProviderMisses, stats.Changes)
	}
}

func TestRoutingCountersClassifierCallsAndHits(t *testing.T) {
	classifier := &routingStub{name: "own", response: "strong"}
	bag := `{"model_routing":{"enabled":true,
		"tiers":[{"name":"strong","model":"m-strong","min_runes":100000}],
		"classifier":{"model":"mini","timeout_ms":900,"cache_ttl_seconds":60}}}`
	l := routingLoop(t, bag, classifier, nil, providers.MasterTenantID)

	l.routeModel(context.Background(), &RunRequest{Message: "same text"})
	l.routeModel(context.Background(), &RunRequest{Message: "same text"})

	stats := l.RoutingStats()
	if stats.ClassifierCalls != 1 || stats.ClassifierCacheHits != 1 {
		t.Errorf("calls=%d hits=%d, want 1 and 1", stats.ClassifierCalls, stats.ClassifierCacheHits)
	}
	if stats.Changes != 2 {
		t.Errorf("changes = %d, want 2 (the cached answer still routes)", stats.Changes)
	}

	broken := &routingStub{name: "own", err: errors.New("boom")}
	l2 := routingLoop(t, bag, broken, nil, providers.MasterTenantID)
	l2.routeModel(context.Background(), &RunRequest{Message: "x"})
	if got := l2.RoutingStats(); got.ClassifierFailures != 1 || got.ClassifierCalls != 1 {
		t.Errorf("failures=%d calls=%d, want 1 and 1", got.ClassifierFailures, got.ClassifierCalls)
	}
}

// An agent with no policy must not allocate counters, and reading its stats must still be safe —
// the WS method reports "cached" for any live Loop, so a zero snapshot is a real answer.
func TestRoutingStatsWithoutPolicy(t *testing.T) {
	l := routingLoop(t, `{"prompt_mode":"full"}`, nil, nil, providers.MasterTenantID)
	if l.routingCounters != nil {
		t.Fatal("no policy means no counters")
	}
	stats := l.RoutingStats()
	if stats.Decisions != 0 || stats.AgentModel != "agent-model" {
		t.Errorf("unexpected snapshot %+v", stats)
	}
	if stats.ByTier == nil {
		t.Error("ByTier must be a non-nil map so the wire shape is stable")
	}
}

func TestRouterRoutingStats(t *testing.T) {
	reg := NewRouter()
	l := routingLoop(t, tierBag(`{"name":"heavy","model":"m-heavy","min_runes":10}`), nil, nil, providers.MasterTenantID)
	l.routeModel(context.Background(), &RunRequest{Message: strings.Repeat("a", 40)})
	reg.Register(l)

	stats, ok := reg.RoutingStats(context.Background(), "router-test")
	if !ok {
		t.Fatal("a registered agent must answer its own stats")
	}
	if stats.Changes != 1 {
		t.Errorf("changes = %d, want 1", stats.Changes)
	}

	// Asking must never resolve: an unknown key reports "not cached" instead of building an agent.
	if _, ok := reg.RoutingStats(context.Background(), "never-resolved"); ok {
		t.Error("unknown agent must report not-cached")
	}
}

func TestClassifierCacheKeyTracksPolicyEdits(t *testing.T) {
	tiers := []store.ModelRoutingTier{{Name: "strong", Model: "m", Hint: "hard tasks"}}
	s := routingSignals{Message: "same message", RunKind: ""}
	cfg := &store.ModelRoutingConfig{Tiers: tiers}

	before := classifierCacheKey(s, cfg)
	edited := &store.ModelRoutingConfig{Tiers: []store.ModelRoutingTier{{Name: "strong", Model: "m", Hint: "REWRITTEN hint"}}}
	if before == classifierCacheKey(s, edited) {
		t.Fatal("cache key must change when the tier list or a hint the model reads changes")
	}
	otherRunKind := s
	otherRunKind.RunKind = "delegation"
	if before == classifierCacheKey(otherRunKind, cfg) {
		t.Fatal("cache key must include the run kind")
	}
}

// Signals must survive a nil counter (rune thresholds still work) and must use the counter when
// it is wired. Counted with the agent's tokenizer by design — see newRoutingSignals.
func TestNewRoutingSignals_TokenCounter(t *testing.T) {
	l := routingLoop(t, `{}`, nil, nil, providers.MasterTenantID)
	msg := "one two three four five six seven"

	if l.tokenCounter == nil {
		t.Fatal("NewLoop must wire a token counter")
	}
	withCounter := newRoutingSignals(l, msg, nil, "")
	if withCounter.Tokens <= 0 {
		t.Errorf("Tokens = %d, want a counted value", withCounter.Tokens)
	}
	if withCounter.Runes != len([]rune(msg)) {
		t.Errorf("Runes = %d, want %d", withCounter.Runes, len([]rune(msg)))
	}

	broke := routingLoop(t, `{}`, nil, nil, providers.MasterTenantID)
	broke.tokenCounter = nil
	if s := newRoutingSignals(broke, msg, nil, ""); s.Tokens != 0 || s.Runes == 0 {
		t.Errorf("nil counter must degrade to runes, got %+v", s)
	}
}
