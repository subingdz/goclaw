package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Compile-time contract: the Postgres provider store really does satisfy the optional
// reader. Without this the only way to discover a mismatch is a runtime type assertion
// that silently reports "backend has no vector columns".
var _ store.EmbeddingSchemaReader = (*PGProviderStore)(nil)

// EmbeddingSchemaInventory lists every pgvector column the database actually has, with
// its width, how many rows already carry a vector, and the HNSW index covering it.
//
// It is a package-level function taking *sql.DB rather than a store method so that code
// which only inspects or migrates the schema does not have to construct a provider
// store — that constructor logs "API key encryption disabled (plain text storage)",
// which would be a false statement in this context.
//
// Discovery comes from the catalog rather than a hard-coded table list on purpose: a
// hand-maintained inventory goes stale the day a migration adds a vector column, and a
// stale inventory in a re-dimension command leaves some column at the old width while
// everything reports success.
func EmbeddingSchemaInventory(ctx context.Context, db *sql.DB) ([]store.VectorColumnInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		  AND format_type(a.atttypid, a.atttypmod) LIKE 'vector%'
		ORDER BY c.relname, a.attname`)
	if err != nil {
		return nil, fmt.Errorf("list vector columns: %w", err)
	}
	defer rows.Close()

	type col struct {
		table, column, typeMod string
	}
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column, &c.typeMod); err != nil {
			return nil, fmt.Errorf("scan vector column: %w", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vector columns: %w", err)
	}

	out := make([]store.VectorColumnInfo, 0, len(cols))
	for _, c := range cols {
		info := store.VectorColumnInfo{Table: c.table, Column: c.column, TypeMod: c.typeMod}

		dims, err := store.ParseVectorTypeMod(c.typeMod)
		if err != nil {
			// Keep the row in the report: a column declared without a dimension is a
			// real problem the operator must see, not something to drop from the list.
			info.Err = err
			out = append(out, info)
			continue
		}
		info.Dimensions = dims

		// Identifiers come from the catalog, but are still quoted so a name that looks
		// like a keyword cannot corrupt the statement.
		q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NOT NULL`,
			quotePgIdent(c.table), quotePgIdent(c.column))
		if err := db.QueryRowContext(ctx, q).Scan(&info.Embedded); err != nil {
			info.Err = fmt.Errorf("count embedded rows in %s.%s: %w", c.table, c.column, err)
		}

		idxes, err := hnswIndexesFor(ctx, db, c.table, c.column)
		if err != nil {
			info.Err = err
		}
		info.Indexes = idxes
		out = append(out, info)
	}
	return out, nil
}

// memoryEmbeddingColumnWidth reads the declared width of the primary memory vector
// column straight from the catalog. It lives here as a package-level function taking
// *sql.DB so the provider store (which gates startup on it) and the memory store (which
// reports it when a write is rejected) answer with the same number instead of each
// keeping its own copy of a constant that the schema can outgrow. The width is read
// with format_type rather than atttypmod so it does not depend on pgvector's internal
// type-modifier encoding.
func memoryEmbeddingColumnWidth(ctx context.Context, db *sql.DB) (int, error) {
	var typ string
	err := db.QueryRowContext(ctx, `
		SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = 'memory_chunks'::regclass
		  AND attname = 'embedding'`).Scan(&typ)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("memory_chunks.embedding not found: is the schema migrated and pgvector installed?")
		}
		return 0, fmt.Errorf("read embedding column type: %w", err)
	}
	dims, err := store.ParseVectorTypeMod(typ)
	if err != nil {
		return 0, err
	}
	if dims > store.MaxIndexableEmbeddingDimensions {
		return dims, fmt.Errorf("vector column is %d-dimensional, above pgvector's %d-dimensional index ceiling: semantic search cannot be indexed",
			dims, store.MaxIndexableEmbeddingDimensions)
	}
	return dims, nil
}

// hnswIndexesFor finds HNSW indexes covering a column, so a re-dimension can drop
// exactly those and replay their own recorded definitions afterwards.
func hnswIndexesFor(ctx context.Context, db *sql.DB, table, column string) ([]store.VectorIndexInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = $1
		  AND indexdef ILIKE '%using hnsw%'
		  AND indexdef LIKE '%(' || $2 || ' vector%'`,
		table, column)
	if err != nil {
		return nil, fmt.Errorf("list hnsw indexes for %s.%s: %w", table, column, err)
	}
	defer rows.Close()

	var out []store.VectorIndexInfo
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return nil, fmt.Errorf("scan hnsw index: %w", err)
		}
		out = append(out, store.VectorIndexInfo{Name: name, Definition: def})
	}
	return out, rows.Err()
}

// quotePgIdent doubles embedded quotes, matching PostgreSQL's own identifier quoting.
func quotePgIdent(s string) string {
	q := "\""
	return q + strings.ReplaceAll(s, q, q+q) + q
}
