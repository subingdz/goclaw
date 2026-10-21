package http

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// embeddingSchemaDims reads the vector column width from the connected backend.
// It returns 0 when the backend implements no vector columns (SQLite/desktop) or when
// the catalog cannot be read: a store we cannot interrogate must not become a reason to
// reject a provider save, because the startup gate and the write path both still report
// a real mismatch.
func (h *ProvidersHandler) embeddingSchemaDims(ctx context.Context) int {
	reader, ok := h.store.(store.EmbeddingSchemaReader)
	if !ok {
		return 0
	}
	dims, err := reader.EmbeddingSchemaDimensions(ctx)
	if err != nil {
		slog.Warn("could not read the memory vector column width", "error", err)
	}
	return dims
}

// validateProviderEmbeddingSettings checks a provider's embedding settings against the
// vector column width this deployment really has. schemaDims comes from the database
// catalog (see store.EmbeddingSchemaReader); 0 means the backend has no vector columns
// at all — SQLite/desktop — so there is no width to agree with.
//
// Two rejections survive a configurable schema, and both stay true for every width:
// a width pgvector can never index, and a width that differs from the column the
// vectors would be written into.
func validateProviderEmbeddingSettings(p *store.LLMProviderData, schemaDims int) error {
	es := store.ParseEmbeddingSettings(p.Settings)
	if es == nil || !es.Enabled {
		return nil
	}
	if es.Dimensions < 0 {
		return fmt.Errorf("embedding.dimensions must be a positive integer or omitted")
	}
	if es.Dimensions > store.MaxIndexableEmbeddingDimensions {
		return fmt.Errorf(
			"embedding.dimensions %d is above pgvector's %d-dimensional index ceiling: vectors that wide are stored but never indexed, so semantic search degrades to a sequential scan",
			es.Dimensions, store.MaxIndexableEmbeddingDimensions,
		)
	}
	if schemaDims > 0 && es.Dimensions > 0 && es.Dimensions != schemaDims {
		return fmt.Errorf(
			"embedding.dimensions %d does not match this database's vector(%d) memory columns; "+
				"use %d, omit the setting, or re-dimension the schema with: goclaw migrate embed-dims --to %d",
			es.Dimensions, schemaDims, schemaDims, es.Dimensions,
		)
	}
	return nil
}
