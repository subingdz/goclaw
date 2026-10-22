package methods

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// agents.routingStats is the read-only window into an agent's live per-turn model-routing counters
// (internal/agent/model_routing.go). It exists so the routing card can answer "is this policy doing
// anything" without anyone grepping logs.

type routingStatsParams struct {
	AgentID string `json:"agentId"`
}

// routingStatsResponse spells out the three states a viewer must not confuse. An agent that has a
// saved policy but has not run since has cached=false and zero counts; an agent with no policy has
// hasPolicy=false. Reporting bare zeros for both would make a fresh save look like a dead policy.
type routingStatsResponse struct {
	AgentID   string              `json:"agentId"`
	HasPolicy bool                `json:"hasPolicy"`
	Cached    bool                `json:"cached"`
	Provider  string              `json:"provider,omitempty"`
	Model     string              `json:"model,omitempty"`
	Stats     *agent.RoutingStats `json:"stats,omitempty"`
}

func (m *AgentsMethods) handleRoutingStats(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params routingStatsParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
			return
		}
	}
	agentKey := strings.TrimSpace(params.AgentID)
	if agentKey == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "agentId is required"))
		return
	}

	// Tenant scoping comes from the store lookup, exactly as the other agents.* handlers get it:
	// a caller who cannot read the agent cannot read its counters either. This also canonicalizes a
	// UUID input to the agent_key the router caches under.
	ag, err := resolveAgentInfo(ctx, m.agentStore, agentKey)
	if err != nil || ag == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, "agent not found: "+agentKey))
		return
	}

	out := routingStatsResponse{
		AgentID:   ag.AgentKey,
		HasPolicy: store.FindModelRoutingConfig(ag.OtherConfig) != nil,
		Provider:  ag.Provider,
		Model:     ag.Model,
	}
	if m.agents == nil {
		client.SendResponse(protocol.NewOKResponse(req.ID, out))
		return
	}
	stats, cached := m.agents.RoutingStats(ctx, ag.AgentKey)
	out.Cached = cached
	if cached {
		out.Stats = &stats
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, out))
}
