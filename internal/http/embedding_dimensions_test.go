package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/crypto"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// These tests cover the guards around a destructive endpoint. The DDL itself needs a live
// pgvector database and is verified there, not by mocking *sql.DB.

func newDimsHandler(t *testing.T) (*ProvidersHandler, *http.ServeMux) {
	t.Helper()
	h := NewProvidersHandler(newMockProviderStore(), newMockSecretsStore(), nil, "")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func postJSON(t *testing.T, mux *http.ServeMux, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embedding/dimensions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestEmbeddingDimensionsRejectsMalformedRequests(t *testing.T) {
	token := setupProvidersAdminToken(t)
	_, mux := newDimsHandler(t)

	tests := []struct {
		name    string
		body    string
		want    int
		wantsub string
	}{
		{"no target", `{"to":0,"confirm":"0"}`, http.StatusBadRequest, "positive"},
		{"negative target", `{"to":-4,"confirm":"-4"}`, http.StatusBadRequest, "positive"},
		{
			// The whole point of confirm: a body that asks for 1024 but confirms 1536 (or
			// nothing at all) must never reach the schema.
			name: "confirmation mismatch", body: `{"to":1024,"confirm":"1536"}`, want: http.StatusBadRequest, wantsub: "confirm must equal",
		},
		{"confirmation missing", `{"to":1024}`, http.StatusBadRequest, "confirm must equal"},
		{"blank confirmation", `{"to":1024,"confirm":"   "}`, http.StatusBadRequest, "confirm must equal"},
		{"unparsable json", `{"to":`, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postJSON(t, mux, token, tt.body)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d, body=%s", w.Code, tt.want, w.Body.String())
			}
			if tt.wantsub != "" && !strings.Contains(w.Body.String(), tt.wantsub) {
				t.Fatalf("body = %q, want it to mention %q", w.Body.String(), tt.wantsub)
			}
		})
	}
}

// A well-formed request on a backend with no vector schema (SQLite/desktop) is refused with
// 503 rather than silently reporting success — the panel must be able to tell the two apart.
func TestEmbeddingDimensionsNeedsVectorSchema(t *testing.T) {
	token := setupProvidersAdminToken(t)
	_, mux := newDimsHandler(t)

	for _, body := range []string{
		`{"to":1024,"confirm":"1024"}`,
		`{"to":1024,"dry_run":true}`,
	} {
		w := postJSON(t, mux, token, body)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("body=%s status = %d, want 503, body=%s", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "no vector schema") {
			t.Fatalf("body = %q, want it to say the backend has no vector schema", w.Body.String())
		}
	}
}

func TestEmbeddingSchemaEndpointNeedsVectorSchema(t *testing.T) {
	token := setupProvidersAdminToken(t)
	_, mux := newDimsHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/embedding/schema", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
}

// The destructive path is master-scope: a tenant admin key must not be able to resize a
// shared database.
func TestEmbeddingDimensionsIsMasterScopeOnly(t *testing.T) {
	tenantKey := "tenant-admin-key"
	setupTestCache(t, map[string]*store.APIKeyData{
		crypto.HashAPIKey(tenantKey): {
			ID:     uuid.New(),
			Scopes: []string{"operator.admin"},
			// A real tenant id, so this is a tenant admin rather than master scope.
			TenantID: uuid.New(),
		},
	})
	_, mux := newDimsHandler(t)

	w := postJSON(t, mux, tenantKey, `{"to":1024,"confirm":"1024"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-master scope, body=%s", w.Code, w.Body.String())
	}
}

func TestEmbeddingStatusCarriesSchemaDimensions(t *testing.T) {
	token := setupProvidersAdminToken(t)
	_, mux := newDimsHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/embedding/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode = %v, body=%s", err, w.Body.String())
	}
	// The mock backend implements no EmbeddingSchemaReader, so the truthful value is 0 and
	// the ceiling is still published for client-side validation.
	if v, ok := out["schema_dimensions"]; !ok || v != float64(0) {
		t.Fatalf("schema_dimensions = %v, want the key present with 0", out["schema_dimensions"])
	}
	if v, ok := out["max_indexable_dimensions"]; !ok || v != float64(store.MaxIndexableEmbeddingDimensions) {
		t.Fatalf("max_indexable_dimensions = %v, want %d", out["max_indexable_dimensions"], store.MaxIndexableEmbeddingDimensions)
	}
}

// The provider save path must reject a width that disagrees with the live column, and the
// error has to name both fixes. schemaAwareProviderStore reports 1536.
func TestProviderSaveSurfacesDimensionRemediation(t *testing.T) {
	token := setupProvidersAdminToken(t)
	mock := &schemaAwareProviderStore{mockProviderStore: newMockProviderStore(), schemaDims: 1536}
	h := NewProvidersHandler(mock, newMockSecretsStore(), nil, "")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	body := `{
		"name": "voyage-dims", "provider_type": "openai_compat",
		"api_base": "https://api.voyageai.com/v1", "enabled": true,
		"settings": {"embedding": {"enabled": true, "model": "voyage-4-nano", "dimensions": 1024}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/providers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"vector(1536)", "goclaw migrate embed-dims --to 1024"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body = %q, want mention of %q", w.Body.String(), want)
		}
	}
	if len(mock.providers) != 0 {
		t.Fatalf("store mutated on a rejected create: %d provider(s)", len(mock.providers))
	}
}

// A width no deployment could ever index is refused even when the backend reports no vector
// columns, so the rejection cannot depend on reading a schema.
func TestProviderSaveRefusesUnindexableWidthWithoutSchema(t *testing.T) {
	token := setupProvidersAdminToken(t)
	mock := newMockProviderStore()
	h := NewProvidersHandler(mock, newMockSecretsStore(), nil, "")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	body := `{
		"name": "wide", "provider_type": "openai_compat",
		"api_base": "https://api.voyageai.com/v1", "enabled": true,
		"settings": {"embedding": {"enabled": true, "model": "m", "dimensions": 4096}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/providers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "index ceiling") {
		t.Fatalf("status = %d body=%s, want 400 mentioning the index ceiling", w.Code, w.Body.String())
	}
}
