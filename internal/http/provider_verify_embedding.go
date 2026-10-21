package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/memory"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// handleVerifyEmbedding tests a provider's embedding capability with a minimal API call.
//
//	POST /v1/providers/{id}/verify-embedding
//	Body: {"model": "text-embedding-3-small"}  (optional, falls back to settings.embedding.model)
//	Response: {"valid": true, "dimensions": 1024, "schema_dimensions": 1024}
//	     or: {"valid": false, "error": "..."}; a provider that answers with a width the
//	     columns do not have also gets "dimension_mismatch" and a "remediation" command.
func (h *ProvidersHandler) handleVerifyEmbedding(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidID, "provider")})
		return
	}

	var req struct {
		Model      string `json:"model"`
		Dimensions int    `json:"dimensions"` // optional: truncate output to N dims
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && err.Error() != "EOF" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidJSON)})
		return
	}

	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": i18n.T(locale, i18n.MsgNotFound, "provider", id.String())})
		return
	}

	// Provider types that cannot serve embeddings
	if store.NoEmbeddingTypes[p.ProviderType] {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": "provider type does not support embeddings"})
		return
	}

	// Parse embedding settings once for model/apiBase/dimensions resolution.
	es := store.ParseEmbeddingSettings(p.Settings)

	// Resolve model: request body → settings.embedding.model → default
	model := req.Model
	if model == "" && es != nil && es.Model != "" {
		model = es.Model
	}
	if model == "" {
		model = "text-embedding-3-small"
	}

	// Resolve API base: settings.embedding.api_base → provider api_base → resolved base
	apiBase := h.resolveAPIBase(p)
	if es != nil && es.APIBase != "" {
		apiBase = es.APIBase
	}

	ep := memory.NewOpenAIEmbeddingProvider(p.Name, p.APIKey, apiBase, model)

	// Apply dimension truncation: request body → provider settings → none.
	// Clamp to reasonable range to avoid sending absurd values upstream.
	truncDims := req.Dimensions
	if truncDims <= 0 && es != nil && es.Dimensions > 0 {
		truncDims = es.Dimensions
	}
	if truncDims > 0 && truncDims <= 8192 {
		ep.WithDimensions(truncDims)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	vectors, embErr := ep.Embed(ctx, []string{"test"})
	if embErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": friendlyVerifyError(embErr)})
		return
	}

	dims := 0
	if len(vectors) > 0 && len(vectors[0]) > 0 {
		dims = len(vectors[0])
	}
	result := map[string]any{"valid": true, "dimensions": dims}
	// Compare against the width this deployment's columns actually have, not against a
	// constant compiled into the binary: the answer the operator needs is "does this
	// agree with my database", and their database is whatever they migrated it to.
	schemaDims := h.embeddingSchemaDims(r.Context())
	result["schema_dimensions"] = schemaDims
	if dims > 0 && schemaDims > 0 && dims != schemaDims {
		result["dimension_mismatch"] = true
		result["remediation"] = fmt.Sprintf("goclaw migrate embed-dims --to %d", dims)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleEmbeddingStatus returns the current embedding system configuration.
//
//	GET /v1/embedding/status
//	Response: {"configured": true, "provider": "openai", "model": "text-embedding-3-small",
//	           "schema_dimensions": 1024, "max_indexable_dimensions": 2000}
//	     or: {"configured": false}
//
// schema_dimensions is read from the database catalog, so the UI can validate a width
// against the deployment it is talking to instead of against a number compiled into the
// page. It is 0 when the backend has no vector columns (SQLite/desktop).
func (h *ProvidersHandler) handleEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	schemaDims := h.embeddingSchemaDims(r.Context())
	status := func(fields map[string]any) {
		fields["schema_dimensions"] = schemaDims
		fields["max_indexable_dimensions"] = store.MaxIndexableEmbeddingDimensions
		writeJSON(w, http.StatusOK, fields)
	}

	// Primary: check system_configs for embedding.provider/model
	if h.sysConfigStore != nil {
		provName, _ := h.sysConfigStore.Get(r.Context(), "embedding.provider")
		model, _ := h.sysConfigStore.Get(r.Context(), "embedding.model")
		if provName != "" {
			if model == "" {
				model = "text-embedding-3-small"
			}
			status(map[string]any{
				"configured":    true,
				"provider":      provName,
				"provider_name": provName,
				"model":         model,
			})
			return
		}
	}

	// Fallback: check provider-level embedding settings
	providerList, err := h.store.ListProviders(r.Context())
	if err != nil {
		status(map[string]any{"configured": false})
		return
	}
	for _, p := range providerList {
		if !p.Enabled || store.NoEmbeddingTypes[p.ProviderType] {
			continue
		}
		es := store.ParseEmbeddingSettings(p.Settings)
		if es != nil && es.Enabled {
			model := es.Model
			if model == "" {
				model = "text-embedding-3-small"
			}
			status(map[string]any{
				"configured":    true,
				"provider":      p.DisplayName,
				"provider_name": p.Name,
				"model":         model,
			})
			return
		}
	}

	status(map[string]any{"configured": false})
}
