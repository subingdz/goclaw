package pg

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// embeddingCacheEntry holds data for a single cache row.
type embeddingCacheEntry struct {
	Hash      string
	Embedding []float32
}

// buildEmbeddingCacheLookup assembles the cache SELECT and its positional args.
// Split out of lookupEmbeddingCache so the dimension filter is testable without a database.
func buildEmbeddingCacheLookup(hashes []string, provider, model string, dims int) (string, []any) {
	placeholders := make([]string, len(hashes))
	args := make([]any, 0, len(hashes)+3)
	for i, h := range hashes {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args = append(args, h)
	}
	args = append(args, provider, model)

	// A cached vector of a different width is a different value, not a reusable one:
	// without this filter a re-dimensioned model keeps hitting old-width rows under the
	// same (hash, provider, model) key, and nothing reports it.
	dimsClause := ""
	if dims > 0 {
		args = append(args, dims)
		dimsClause = fmt.Sprintf(" AND dims = $%d", len(args))
	}

	query := fmt.Sprintf(
		"SELECT hash, embedding FROM embedding_cache WHERE hash IN (%s) AND provider = $%d AND model = $%d%s",
		strings.Join(placeholders, ","), len(hashes)+1, len(hashes)+2, dimsClause,
	)
	return query, args
}

// lookupEmbeddingCache fetches cached embeddings for the given content hashes.
// Returns a map from hash -> embedding vector. Missing hashes are simply absent.
func (s *PGMemoryStore) lookupEmbeddingCache(ctx context.Context, hashes []string, provider, model string, dims int) (map[string][]float32, error) {
	if len(hashes) == 0 {
		return nil, nil
	}

	query, args := buildEmbeddingCacheLookup(hashes, provider, model, dims)

	type cacheRow struct {
		Hash      string `db:"hash"`
		Embedding string `db:"embedding"`
	}
	var cacheRows []cacheRow
	if err := pkgSqlxDB.SelectContext(ctx, &cacheRows, query, args...); err != nil {
		return nil, fmt.Errorf("lookup embedding cache: %w", err)
	}

	result := make(map[string][]float32, len(hashes))
	for _, row := range cacheRows {
		vec, err := parseVector(row.Embedding)
		if err != nil {
			slog.Warn("embedding cache parse error", "hash", row.Hash, "error", err)
			continue
		}
		if dims > 0 && len(vec) != dims {
			// Belt and braces: the SQL filter should already have excluded this row.
			slog.Warn("embedding cache row width mismatch, discarding",
				"hash", row.Hash, "provider", provider, "model", model,
				"expected_dims", dims, "actual_dims", len(vec))
			continue
		}
		result[row.Hash] = vec
	}
	return result, nil
}

// writeEmbeddingCache batch-upserts embedding cache entries. Entries whose width does
// not match this deployment's vector column are skipped, never silently.
func (s *PGMemoryStore) writeEmbeddingCache(ctx context.Context, entries []embeddingCacheEntry, provider, model string) error {
	if len(entries) == 0 {
		return nil
	}

	now := time.Now()
	tenantID := tenantIDForInsert(ctx)

	// Process in batches of 100 to avoid exceeding max query params
	const batchSize = 100
	for start := 0; start < len(entries); start += batchSize {
		end := min(start+batchSize, len(entries))
		batch := entries[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO embedding_cache (hash, provider, model, embedding, dims, created_at, updated_at, tenant_id) VALUES `)
		args := make([]any, 0, len(batch)*7)
		for i, e := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			base := i * 7
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d::vector,$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+6, base+7)
			args = append(args, e.Hash, provider, model, vectorToString(e.Embedding), len(e.Embedding), now, tenantID)
		}
		sb.WriteString(` ON CONFLICT (hash, provider, model) DO UPDATE SET embedding = EXCLUDED.embedding, dims = EXCLUDED.dims, updated_at = EXCLUDED.updated_at`)

		_, err := s.db.ExecContext(ctx, sb.String(), args...)
		if err != nil {
			// pgvector dimension mismatch — the row cannot be stored. Skipping is correct,
			// staying silent is not: without this line a misconfigured dimensions value
			// shows up only as "search quality got worse", never as an error.
			if strings.Contains(err.Error(), "dimensions") {
				// Report the width the column actually has. A build-time constant would
				// be wrong exactly in the situation this warning exists to describe.
				schemaDims, widthErr := memoryEmbeddingColumnWidth(ctx, s.db)
				attrs := []any{
					"provider", provider, "model", model,
					"actual_dims", len(batch[0].Embedding),
					"schema_dims", schemaDims,
					"hint", "embedding.dimensions does not match the vector column width; " +
						"align the provider setting or run `goclaw migrate embed-dims --to <dimensions>`",
					"error", err,
				}
				if widthErr != nil {
					attrs = append(attrs, "schema_dims_error", widthErr.Error())
				}
				slog.Warn("embedding cache skipped: vector dimension mismatch", attrs...)
				return nil
			}
			return fmt.Errorf("batch write embedding cache: %w", err)
		}
	}
	return nil
}

// parseVector converts a pgvector string like "[0.1,0.2,0.3]" into []float32.
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return nil, fmt.Errorf("vector string too short: %q", s)
	}
	// Strip surrounding brackets ([] from pgvector, () as fallback)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	s = strings.TrimPrefix(s, "(")
	s = strings.TrimSuffix(s, ")")
	if s == "" {
		return nil, nil
	}

	parts := strings.Split(s, ",")
	vec := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("parse vector element %q: %w", p, err)
		}
		vec = append(vec, float32(f))
	}
	return vec, nil
}
