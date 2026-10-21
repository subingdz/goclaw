package pg

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// These cases are decided before any query runs, so they need no database: the point is
// that an impossible target never reaches DDL.
func TestRedimensionPlanValidatesTargetFirst(t *testing.T) {
	tests := []struct {
		name    string
		target  int
		wantErr string
	}{
		{"zero", 0, "positive"},
		{"negative", -1, "positive"},
		{"above the index ceiling", store.MaxIndexableEmbeddingDimensions + 1, "index ceiling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := RedimensionPlan(context.Background(), nil, tt.target)
			if err == nil {
				t.Fatalf("RedimensionPlan(%d) = nil error, want rejection", tt.target)
			}
			if report != nil {
				t.Fatalf("RedimensionPlan(%d) returned a report before reading anything", tt.target)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRedimensionPropagatesPlanRejection(t *testing.T) {
	// force=true must not rescue an invalid target: the guard is in the plan, which both the
	// CLI and the HTTP endpoint call first.
	_, err := Redimension(context.Background(), nil, 4096, true, false)
	if err == nil || !strings.Contains(err.Error(), "index ceiling") {
		t.Fatalf("Redimension(4096, force) error = %v, want the ceiling rejection", err)
	}
}

// The report has to be usable by a panel: a column whose width cannot be read stays visible
// with its error text, and ErrVectorsPresent must be comparable with errors.Is.
func TestRedimensionColumnMarshalsErrorAsText(t *testing.T) {
	c := RedimensionColumn{Table: "memory_chunks", Column: "embedding", From: 0, To: 1024,
		Action: ActionRedimension, Err: errors.New("vector column has no declared dimension")}

	data, err := c.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	if !strings.Contains(string(data), "no declared dimension") {
		t.Fatalf("marshalled column = %s, want the error text visible", data)
	}

	// A clean column must not grow an empty error key that a UI then renders.
	if data, err = (RedimensionColumn{Table: "t", Column: "c"}).MarshalJSON(); err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	} else if strings.Contains(string(data), `"error":""`) {
		t.Fatalf("marshalled column = %s, want no empty error field", data)
	}
}

// Connecting to an unusable host proves the inventory query is what fails next, and that the
// error is surfaced rather than swallowed.
func TestRedimensionPlanSurfacesConnectionFailure(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://nobody:none@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Skipf("sql.Open unavailable: %v", err)
	}
	defer db.Close()

	if _, err := RedimensionPlan(context.Background(), db, 1024); err == nil {
		t.Fatal("RedimensionPlan() = nil error against an unreachable database, want a failure")
	}
}
