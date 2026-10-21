package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// This file is the single implementation of "change the width of every pgvector column".
// Both entry points use it: `goclaw migrate embed-dims` (cmd) and the operator-facing
// admin endpoint (internal/http). That duplication of *access* without duplication of
// *behaviour* is deliberate — a panel button and a CLI flag must refuse for the same
// reasons, or the safer of the two becomes the one people actually use.
//
// The per-column procedure is: drop the covering HNSW indexes, clear the column, force it
// to store in-line, re-type it, then replay each recorded index definition — all inside
// one transaction, so a failure midway leaves the table exactly as it was.
//
// These are DDL statements, so the provenance of every interpolated value matters: table,
// column and index names come from pg_catalog (pg_class / pg_attribute / pg_indexes) via
// EmbeddingSchemaInventory and are escaped by quotePgIdent; the index definitions are
// server-generated strings read back from pg_indexes; `target` is an integer validated
// positive and bounded by store.MaxIndexableEmbeddingDimensions before any statement is
// assembled. Nothing request-controlled is concatenated into SQL — including the HTTP path,
// whose body contributes only that integer and two booleans.

// RedimensionAction is what Redimension intends to do with one column.
type RedimensionAction string

const (
	// ActionNoop: the column already has the requested width.
	ActionNoop RedimensionAction = "noop"
	// ActionRedimension: the column will be cleared and re-typed.
	ActionRedimension RedimensionAction = "redimension"
)

// RedimensionColumn is one column's entry in a plan or a result. Action describes intent;
// Err carries either a schema problem found while planning or the failure that aborted
// this column's transaction.
type RedimensionColumn struct {
	Table   string            `json:"table"`
	Column  string            `json:"column"`
	From    int               `json:"from"`
	To      int               `json:"to"`
	Applied bool              `json:"applied"`
	Indexes []string          `json:"indexes,omitempty"`
	Action  RedimensionAction `json:"action"`
	Err     error             `json:"-"`
}

// MarshalJSON exposes Err as text so an HTTP response can show it instead of swallowing it.
func (c RedimensionColumn) MarshalJSON() ([]byte, error) {
	type alias RedimensionColumn
	out := struct {
		alias
		Error string `json:"error,omitempty"`
	}{alias: alias(c)}
	if c.Err != nil {
		out.Error = c.Err.Error()
	}
	return json.Marshal(out)
}

// RedimensionReport is the whole-database view: every vector column, how many rows would
// be destroyed, and whether the operation is currently allowed.
type RedimensionReport struct {
	Target       int                 `json:"target"`
	Columns      []RedimensionColumn `json:"columns"`
	EmbeddedRows int                 `json:"embedded_rows"`
	NeedsForce   bool                `json:"needs_force"`
	Unreadable   int                 `json:"unreadable"`
}

// ErrVectorsPresent is returned when a plan would destroy stored vectors and the caller did
// not say it may. Judge it with errors.Is; the per-column detail is on the report.
var ErrVectorsPresent = errors.New("columns already hold vectors")

// ValidateTarget rejects the widths that can never work, before anything is read or written.
// The CLI calls it before connecting so `--to 4096` does not turn into a DSN error, and
// RedimensionPlan calls it again because the HTTP path has no reason to know the caller
// already checked. One definition, both entry points.
func ValidateTarget(target int) error {
	if target <= 0 {
		return fmt.Errorf("target dimension must be a positive integer, got %d", target)
	}
	if target > store.MaxIndexableEmbeddingDimensions {
		return fmt.Errorf("target %d exceeds pgvector's %d-dimensional index ceiling: vectors that wide are stored but never indexed, so semantic search would degrade to a sequential scan",
			target, store.MaxIndexableEmbeddingDimensions)
	}
	return nil
}

// RedimensionPlan inspects the live schema and reports what a re-dimension to target would
// do, without touching anything. It is the dry-run path for the CLI and the panel alike.
func RedimensionPlan(ctx context.Context, db *sql.DB, target int) (*RedimensionReport, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}

	cols, err := EmbeddingSchemaInventory(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("no pgvector columns found: is this PostgreSQL with pgvector installed and the schema migrated?")
	}

	report := &RedimensionReport{Target: target, Columns: make([]RedimensionColumn, 0, len(cols))}
	for _, c := range cols {
		entry := RedimensionColumn{Table: c.Table, Column: c.Column, From: c.Dimensions, To: target, Err: c.Err}
		for _, idx := range c.Indexes {
			entry.Indexes = append(entry.Indexes, idx.Name)
		}

		if c.Err != nil {
			// A column whose width cannot be read is a real schema problem. It is kept in
			// the report and it blocks the operation: proceeding would leave that column at
			// the old width while everything else reports success.
			report.Unreadable++
			report.Columns = append(report.Columns, entry)
			continue
		}
		report.EmbeddedRows += c.Embedded
		if c.Dimensions == target {
			entry.Action = ActionNoop
		} else {
			entry.Action = ActionRedimension
		}
		report.Columns = append(report.Columns, entry)
	}

	if report.Unreadable > 0 {
		bad := make([]string, 0, report.Unreadable)
		for _, e := range report.Columns {
			if e.Err != nil {
				bad = append(bad, e.Table+"."+e.Column)
			}
		}
		return report, fmt.Errorf("cannot proceed: %d column(s) have unreadable widths (%s); fix the schema before re-dimensioning",
			report.Unreadable, strings.Join(bad, ", "))
	}
	report.NeedsForce = report.EmbeddedRows > 0
	return report, nil
}

// Redimension performs the plan. When the database already holds vectors, force must be
// true or nothing is modified and ErrVectorsPresent is returned alongside the report.
func Redimension(ctx context.Context, db *sql.DB, target int, force, dryRun bool) (*RedimensionReport, error) {
	report, err := RedimensionPlan(ctx, db, target)
	if err != nil {
		return report, err
	}
	if dryRun {
		return report, nil
	}
	if report.NeedsForce && !force {
		return report, fmt.Errorf("%w: %d row(s) hold vectors and re-dimensioning clears them; re-run with force to let the memory backfill regenerate them (that costs embedding API calls proportional to the stored chunks)",
			ErrVectorsPresent, report.EmbeddedRows)
	}

	// Apply per column, not all-at-once: one column's index rebuild can fail on its own
	// data, and a partial success is far more useful to an operator than a rolled-back
	// everything. Each column is its own transaction, so no column is ever left indexed at
	// the wrong width.
	for i := range report.Columns {
		e := &report.Columns[i]
		if e.Action != ActionRedimension {
			continue
		}
		info := store.VectorColumnInfo{Table: e.Table, Column: e.Column, Dimensions: e.From}
		idxes, err := hnswIndexesFor(ctx, db, e.Table, e.Column)
		if err != nil {
			e.Err = err
			continue
		}
		info.Indexes = idxes
		if err := redimensionColumn(ctx, db, info, target); err != nil {
			e.Err = err
			continue
		}
		e.Applied = true
	}

	var failed []string
	for _, e := range report.Columns {
		if e.Err != nil {
			failed = append(failed, e.Table+"."+e.Column+": "+e.Err.Error())
		}
	}
	if len(failed) > 0 {
		return report, fmt.Errorf("%d column(s) failed: %s", len(failed), strings.Join(failed, "; "))
	}
	return report, nil
}

// redimensionColumn runs one column's transaction.
//
// The UPDATE ... SET NULL is not cosmetic: PostgreSQL treats two vector() typmods as the
// same binary-coercible type, so ALTER COLUMN TYPE may only rewrite atttypmod and never
// look at the stored values. A 1536-wide value left in a column re-declared as
// vector(1024) would survive until the index build — or, worse, until search started
// returning nonsense. Nulling the column first makes the outcome identical whether
// PostgreSQL relabels or rewrites.
//
// SET STORAGE PLAIN is load-bearing, not a tuning hint: without it a large vector is
// compressed and moved out to TOAST pages, and PostgreSQL does not refresh a toasted value
// when a column is re-typed — so a row could keep reporting a 1536-wide vector under a
// vector(1024) declaration. Re-typing while the column stores its values in-line means the
// new declaration is true of every row before the first vector is written, which is what
// the HNSW index rebuilt below relies on.
func redimensionColumn(ctx context.Context, db *sql.DB, c store.VectorColumnInfo, target int) error {
	tbl, col := quotePgIdent(c.Table), quotePgIdent(c.Column)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, idx := range c.Indexes {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP INDEX IF EXISTS %s", quotePgIdent(idx.Name))); err != nil {
			return fmt.Errorf("drop index %s: %w", idx.Name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s = NULL", tbl, col)); err != nil {
		return fmt.Errorf("clear vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET STORAGE PLAIN", tbl, col)); err != nil {
		return fmt.Errorf("set storage plain: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE vector(%d)", tbl, col, target)); err != nil {
		return fmt.Errorf("alter column type: %w", err)
	}
	for _, idx := range c.Indexes {
		if _, err := tx.ExecContext(ctx, idx.Definition); err != nil {
			return fmt.Errorf("replay index %q: %w", idx.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
