package methods

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// stubRoutingStatsAgent answers the optional RoutingStatsAgent interface, so the handler can be
// tested against a live router entry without building a whole Loop (and without a provider).
type stubRoutingStatsAgent struct {
	cacheHitStubAgent
	stats agent.RoutingStats
}

func (s *stubRoutingStatsAgent) RoutingStats() agent.RoutingStats { return s.stats }

// routingStatsAgentStore implements only the two reads resolveAgentInfo uses; every other method
// stays on the nil embed, so an unexpected call path panics instead of passing silently.
type routingStatsAgentStore struct {
	store.AgentStore
	data *store.AgentData
}

func (s routingStatsAgentStore) GetByKey(context.Context, string) (*store.AgentData, error) {
	return s.data, nil
}

func (s routingStatsAgentStore) GetByID(context.Context, uuid.UUID) (*store.AgentData, error) {
	return s.data, nil
}

func routingStatsData() *store.AgentData {
	return &store.AgentData{
		BaseModel: store.BaseModel{ID: store.GenNewID()},
		AgentKey:  "support",
		Provider:  "openai",
		Model:     "gpt-mini",
		OtherConfig: json.RawMessage(
			`{"model_routing":{"enabled":true,"tiers":[{"model":"m-heavy","min_runes":100}]}}`),
	}
}

func callRoutingStats(t *testing.T, m *AgentsMethods, params map[string]any) (protocol.ResponseFrame, routingStatsResponse) {
	t.Helper()
	client, ch := gateway.NewCapturingTestClient(permissions.RoleAdmin, uuid.Nil, "owner", 2)
	ctx := store.WithTenantID(context.Background(), uuid.Nil)

	m.handleRoutingStats(ctx, client, sessionReqFrame(t, protocol.MethodAgentsRoutingStats, params))

	var raw []byte
	select {
	case raw = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no response frame")
	}
	var resp struct {
		OK      bool                 `json:"ok"`
		Error   *protocol.ErrorShape `json:"error"`
		Payload routingStatsResponse `json:"payload"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return protocol.ResponseFrame{OK: resp.OK, Error: resp.Error}, resp.Payload
}

func TestRoutingStatsRejectsMissingAgentID(t *testing.T) {
	m := NewAgentsMethods(agent.NewRouter(), &config.Config{}, "", "", nil, nil, nil)

	resp, _ := callRoutingStats(t, m, map[string]any{})
	if resp.Error == nil || resp.Error.Code != protocol.ErrInvalidRequest {
		t.Fatalf("error = %+v, want INVALID_REQUEST", resp.Error)
	}
}

// The store lookup is the tenant gate: a caller who cannot read the agent cannot read its counters,
// and an unknown key must not quietly answer "zero stats".
func TestRoutingStatsUnknownAgentIsNotFound(t *testing.T) {
	m := NewAgentsMethods(agent.NewRouter(), &config.Config{}, "", "",
		&errorAgentStore{err: errors.New("agent not found")}, nil, nil)

	resp, _ := callRoutingStats(t, m, map[string]any{"agentId": "ghost"})
	if resp.Error == nil || resp.Error.Code != protocol.ErrNotFound {
		t.Fatalf("error = %+v, want NOT_FOUND", resp.Error)
	}
}

func TestRoutingStatsReportsSavedButNotLive(t *testing.T) {
	data := routingStatsData()
	m := NewAgentsMethods(agent.NewRouter(), &config.Config{}, "", "",
		routingStatsAgentStore{data: data}, nil, nil)

	_, got := callRoutingStats(t, m, map[string]any{"agentId": "support"})
	if !got.HasPolicy {
		t.Error("hasPolicy must be true for a saved policy")
	}
	if got.Cached || got.Stats != nil {
		t.Errorf("cached=%v stats=%+v, want the \"saved but not live\" shape", got.Cached, got.Stats)
	}
	if got.AgentID != "support" || got.Model != "gpt-mini" {
		t.Errorf("identity = %+v", got)
	}
}

func TestRoutingStatsReturnsLiveCounters(t *testing.T) {
	data := routingStatsData()
	rt := agent.NewRouter()
	rt.Register(&stubRoutingStatsAgent{
		cacheHitStubAgent: cacheHitStubAgent{id: data.AgentKey, uid: data.ID},
		stats:             agent.RoutingStats{Decisions: 7, Changes: 3, ByTier: map[string]int64{"heavy": 3}},
	})
	m := NewAgentsMethods(rt, &config.Config{}, "", "", routingStatsAgentStore{data: data}, nil, nil)

	// A UUID input must canonicalize to the agent_key the router caches under.
	_, got := callRoutingStats(t, m, map[string]any{"agentId": data.ID.String()})
	if !got.Cached || got.Stats == nil {
		t.Fatalf("cached=%v stats=%+v, want live counters", got.Cached, got.Stats)
	}
	if got.Stats.Decisions != 7 || got.Stats.Changes != 3 || got.Stats.ByTier["heavy"] != 3 {
		t.Errorf("stats = %+v", got.Stats)
	}
	if !got.HasPolicy {
		t.Error("hasPolicy must be reported alongside live stats")
	}
}

// An agent with no policy is a valid answer with cached=false — the card must be able to tell
// "routing is off" from "routing is on but has not run since it was saved".
func TestRoutingStatsNoPolicyAgent(t *testing.T) {
	data := routingStatsData()
	data.OtherConfig = json.RawMessage(`{"prompt_mode":"full"}`)
	m := NewAgentsMethods(agent.NewRouter(), &config.Config{}, "", "",
		routingStatsAgentStore{data: data}, nil, nil)

	_, got := callRoutingStats(t, m, map[string]any{"agentId": "support"})
	if got.HasPolicy {
		t.Error("hasPolicy must be false without a model_routing block")
	}
	if got.Cached {
		t.Error("nothing is cached in this fixture")
	}
}
