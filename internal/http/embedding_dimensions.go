package http

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// Embedding dimension control. The width of the memory vector columns is a property of the
// database, so these endpoints do not store a preference anywhere: GET reports what the
// schema says, POST changes the schema through the same pg.Redimension the CLI calls.
//
// Destructive by nature — re-dimensioning clears every stored vector — so it is gated three
// ways: master scope (a tenant admin must not resize a shared database), Admin role, and a
// confirm field that must equal the requested width as text. The CLI's --force has the same
// shape here as `force`, and a dry run is the default path the UI is expected to take first.

// SetDB gives the provider handler the raw database handle needed for DDL. It is a setter
// rather than a constructor parameter because most ProvidersHandler call sites (and every
// test) never touch the schema, and a non-PG backend has nothing to hand over.
func (h *ProvidersHandler) SetDB(db *sql.DB) { h.db = db }

// schemaColumn is the wire shape for one vector column. store.VectorColumnInfo carries an
// error value, which would serialise as an opaque object, so the text is lifted out here.
type schemaColumn struct {
	Table      string   `json:"table"`
	Column     string   `json:"column"`
	TypeMod    string   `json:"type_mod"`
	Dimensions int      `json:"dimensions"`
	Embedded   int      `json:"embedded"`
	Indexes    []string `json:"indexes,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// schemaResponse is the read-only view used by the panel.
type schemaResponse struct {
	SchemaDimensions       int            `json:"schema_dimensions"`
	MaxIndexableDimensions int            `json:"max_indexable_dimensions"`
	Columns                []schemaColumn `json:"columns"`
	Error                  string         `json:"error,omitempty"`
}

func (h *ProvidersHandler) handleEmbeddingSchema(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !requireMasterScope(w, r) {
		return
	}
	resp := schemaResponse{MaxIndexableDimensions: store.MaxIndexableEmbeddingDimensions}
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": i18n.T(locale, i18n.MsgInvalidRequest, "this backend exposes no vector schema"),
		})
		return
	}

	cols, err := pg.EmbeddingSchemaInventory(r.Context(), h.db)
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	resp.Columns = make([]schemaColumn, 0, len(cols))
	for _, c := range cols {
		entry := schemaColumn{
			Table: c.Table, Column: c.Column, TypeMod: c.TypeMod,
			Dimensions: c.Dimensions, Embedded: c.Embedded,
		}
		for _, idx := range c.Indexes {
			entry.Indexes = append(entry.Indexes, idx.Name)
		}
		if c.Err != nil {
			entry.Error = c.Err.Error()
		}
		resp.Columns = append(resp.Columns, entry)
	}
	// schema_dimensions reports the memory_chunks column specifically, matching the value
	// /v1/embedding/status already returns, so the panel has one number for "the width we
	// are". Zero means this backend has no vector columns at all.
	resp.SchemaDimensions = h.embeddingSchemaDims(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

type redimensionRequest struct {
	To      int    `json:"to"`
	Confirm string `json:"confirm"`
	Force   bool   `json:"force"`
	DryRun  bool   `json:"dry_run"`
}

func (h *ProvidersHandler) handleEmbeddingDimensions(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	if !requireMasterScope(w, r) {
		return
	}

	var req redimensionRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "read body")})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidJSON)})
		return
	}

	// Rejected on the request alone, so a malformed attempt costs nothing and cannot reach
	// the schema at all.
	if req.To <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": i18n.T(locale, i18n.MsgInvalidRequest, "to must be a positive dimension"),
		})
		return
	}
	// The confirm value must repeat the width as text. It is not decoration: the operation
	// deletes every stored vector, and a form that submits without an explicit typed value
	// is a form somebody will fire by accident.
	if !req.DryRun && strings.TrimSpace(req.Confirm) != strconv.Itoa(req.To) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": i18n.T(locale, i18n.MsgInvalidRequest,
				fmt.Sprintf("confirm must equal the target dimension as text (%d)", req.To)),
		})
		return
	}

	// Only now that the request is well-formed: a backend without a vector schema (SQLite)
	// has nothing to re-dimension.
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": i18n.T(locale, i18n.MsgInvalidRequest, "this backend exposes no vector schema"),
		})
		return
	}

	report, err := pg.Redimension(r.Context(), h.db, req.To, req.Force, req.DryRun)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, pg.ErrVectorsPresent):
			// The caller needs the counts to ask for force intelligently, so the report is
			// returned with the refusal rather than swallowed.
			status = http.StatusConflict
		case strings.Contains(err.Error(), "unreadable"), strings.Contains(err.Error(), "ceiling"),
			strings.Contains(err.Error(), "positive"), strings.Contains(err.Error(), "no pgvector columns"):
			status = http.StatusBadRequest
		}
		h.writeRedimensionResult(w, report, status, err.Error())
		return
	}

	applied := 0
	for _, c := range report.Columns {
		if c.Applied {
			applied++
		}
	}
	if applied > 0 {
		slog.Warn("security.embedding.redimension",
			"to", report.Target, "columns", applied, "cleared_rows", report.EmbeddedRows,
			"actor", store.UserIDFromContext(r.Context()))
	}
	h.writeRedimensionResult(w, report, http.StatusOK, "")
}

func (h *ProvidersHandler) writeRedimensionResult(w http.ResponseWriter, report *pg.RedimensionReport, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if report == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	out := struct {
		*pg.RedimensionReport
		Error string `json:"error,omitempty"`
	}{report, msg}
	_ = json.NewEncoder(w).Encode(out)
}
