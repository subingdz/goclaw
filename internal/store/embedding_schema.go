package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// EmbeddingSchemaReader reports the vector column width the memory schema actually
// uses. It is an optional capability on purpose: a backend without vector columns
// (SQLite) simply does not implement it, which is the truthful answer, rather than
// returning 0 and letting callers mistake "absent" for "a real width of zero".
//
// The schema, not the config, is the source of truth: a stored width in a config
// file can disagree with the column and nothing would notice until a vector write
// failed — and that failure is currently swallowed on the search path.
type EmbeddingSchemaReader interface {
	EmbeddingSchemaDimensions(ctx context.Context) (int, error)
}

// MaxIndexableEmbeddingDimensions is pgvector's ceiling for HNSW and IVFFlat indexes.
// Wider vectors can be stored but never indexed, so semantic search degrades into a
// sequential scan while still returning results — the kind of failure nobody notices.
const MaxIndexableEmbeddingDimensions = 2000

// VectorIndexInfo names one HNSW index covering a vector column, together with the
// definition needed to recreate it after the column type changes.
type VectorIndexInfo struct {
	Name       string
	Definition string
}

// VectorColumnInfo is one discovered pgvector column and the facts a re-dimension
// operation needs about it. TypeMod keeps the raw catalog text so a caller can see
// what the column really says even when it cannot be parsed (e.g. "vector").
type VectorColumnInfo struct {
	Table      string
	Column     string
	TypeMod    string
	Dimensions int
	Embedded   int
	Indexes    []VectorIndexInfo
	Err        error
}

// ParseVectorTypeMod extracts the dimension count from a PostgreSQL type description
// such as "vector(1536)". It rejects the dimensionless form ("vector") because an
// un-dimensioned column cannot be indexed at all.
func ParseVectorTypeMod(s string) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty vector type description")
	}
	if !strings.HasPrefix(t, "vector") {
		return 0, fmt.Errorf("column is not a pgvector column: %q", t)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(t, "vector"))
	if rest == "" {
		return 0, fmt.Errorf("vector column has no declared dimension (%q); it cannot be indexed", t)
	}
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return 0, fmt.Errorf("unrecognised vector type description: %q", t)
	}
	digits := strings.TrimSpace(rest[1 : len(rest)-1])
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("unparsable vector dimension in %q: %w", t, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("vector dimension must be positive, got %d", n)
	}
	return n, nil
}
