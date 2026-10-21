package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/memory"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// --- Red Team F3: subagent ExecTool must inherit secureCLIStore wiring ---

type stubSecureCLIStoreCmd struct{}

func (s *stubSecureCLIStoreCmd) Create(ctx context.Context, b *store.SecureCLIBinary) error {
	return nil
}
func (s *stubSecureCLIStoreCmd) Get(ctx context.Context, id uuid.UUID) (*store.SecureCLIBinary, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) Update(ctx context.Context, id uuid.UUID, updates map[string]any) error {
	return nil
}
func (s *stubSecureCLIStoreCmd) Delete(ctx context.Context, id uuid.UUID) error { return nil }
func (s *stubSecureCLIStoreCmd) List(ctx context.Context) ([]store.SecureCLIBinary, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) ListEnabled(ctx context.Context) ([]store.SecureCLIBinary, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) ListForAgent(ctx context.Context, agentID uuid.UUID) ([]store.SecureCLIBinary, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) IsRegisteredBinary(ctx context.Context, binaryName string) (bool, error) {
	return false, nil
}
func (s *stubSecureCLIStoreCmd) LookupByBinary(ctx context.Context, binaryName string, agentID *uuid.UUID, userID string) (*store.SecureCLIBinary, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) GetUserCredentials(ctx context.Context, binaryID uuid.UUID, userID string) (*store.SecureCLIUserCredential, error) {
	return nil, nil
}
func (s *stubSecureCLIStoreCmd) SetUserCredentials(ctx context.Context, binaryID uuid.UUID, userID string, encryptedEnv []byte) error {
	return nil
}
func (s *stubSecureCLIStoreCmd) SetUserCredentialsTyped(ctx context.Context, binaryID uuid.UUID, userID string, encryptedEnv []byte, credentialType, hostScope *string) error {
	return nil
}
func (s *stubSecureCLIStoreCmd) DeleteUserCredentials(ctx context.Context, binaryID uuid.UUID, userID string) error {
	return nil
}
func (s *stubSecureCLIStoreCmd) ListUserCredentials(ctx context.Context, binaryID uuid.UUID) ([]store.SecureCLIUserCredential, error) {
	return nil, nil
}

// TestSubagentExecTool_StoreWired ensures the subagent tool factory wires the
// SecureCLIStore into the subagent's ExecTool, so the gate enforces on
// spawned-subagent exec (Red Team F3). A missing wiring would let a parent
// agent bypass the gate by delegating the exec to a subagent.
func TestSubagentExecTool_StoreWired(t *testing.T) {
	parent := tools.NewRegistry()
	stub := &stubSecureCLIStoreCmd{}

	_, execTool := buildSubagentToolsRegistry(parent, t.TempDir(), false, nil, stub)
	if execTool == nil {
		t.Fatal("expected non-nil exec tool from factory")
	}
	if !execTool.HasSecureCLIStore() {
		t.Fatalf("expected subagent ExecTool to have SecureCLIStore wired (Red Team F3)")
	}
}

// TestSubagentExecTool_NilStoreIsSafe ensures the factory does not panic when
// the store is unavailable (Lite edition / no encryption key). The exec tool
// simply lacks the gate — same as today's Lite behavior.
func TestSubagentExecTool_NilStoreIsSafe(t *testing.T) {
	parent := tools.NewRegistry()
	_, execTool := buildSubagentToolsRegistry(parent, t.TempDir(), false, nil, nil)
	if execTool == nil {
		t.Fatal("expected non-nil exec tool from factory")
	}
	if execTool.HasSecureCLIStore() {
		t.Fatalf("expected no SecureCLIStore when passed nil (Lite path)")
	}
}

func captureEmbeddingRequest(t *testing.T, es *store.EmbeddingSettings, schemaDims int) map[string]any {
	t.Helper()

	// Pin Docker detection off so the loopback httptest URL is not rewritten
	// to host.docker.internal when this suite runs inside a container.
	t.Cleanup(config.SetInDockerForTest(false))

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		// Answer at the width that was asked for, so the fixture cannot hide a request
		// that the real model would have refused.
		width := 1536.0
		if d, ok := requestBody["dimensions"].(float64); ok && d > 0 {
			width = d
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"embedding": make([]float32, int(width)),
				"index":     0,
			}},
		})
	}))
	defer server.Close()

	provider := &store.LLMProviderData{
		Name:         "embedding-provider",
		ProviderType: store.ProviderOpenAICompat,
		APIKey:       "test-key",
		APIBase:      server.URL,
		Enabled:      true,
	}

	ep := buildEmbeddingProvider(provider, es, nil, nil, schemaDims)
	if ep == nil {
		t.Fatal("buildEmbeddingProvider() = nil, want provider")
	}
	if _, err := ep.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	return requestBody
}

// An empty embedding.dimensions must mean "send nothing": several providers reject the
// parameter outright for models whose native width already matches the column (SiliconFlow
// answers "The parameter is invalid. Please check again." for BAAI/bge-m3), so forcing the
// schema width here would make such a model unusable.
func TestBuildEmbeddingProviderSendsNoTruncationWhenUndeclared(t *testing.T) {
	for _, schemaDims := range []int{0, 1024, 1536} {
		requestBody := captureEmbeddingRequest(t, nil, schemaDims)
		if _, ok := requestBody["dimensions"]; ok {
			t.Fatalf("schemaDims=%d: request carries dimensions=%v, want no truncation parameter",
				schemaDims, requestBody["dimensions"])
		}
	}
}

// A declared width is passed through untouched — that is the operator asking for
// truncation, e.g. text-embedding-3-large down to the column's 1024.
func TestBuildEmbeddingProviderSendsDeclaredTruncation(t *testing.T) {
	requestBody := captureEmbeddingRequest(t, &store.EmbeddingSettings{
		Enabled:    true,
		Model:      "text-embedding-3-large",
		Dimensions: 1024,
	}, 1024)
	if got := requestBody["dimensions"]; got != float64(1024) {
		t.Fatalf("dimensions = %v, want the declared 1024", got)
	}
}

type embeddingProviderStoreStub struct {
	masterProviders []store.LLMProviderData
	allProviders    []store.LLMProviderData
	listTenant      uuid.UUID
	listAllCalled   bool
}

func (s *embeddingProviderStoreStub) CreateProvider(context.Context, *store.LLMProviderData) error {
	return nil
}
func (s *embeddingProviderStoreStub) GetProvider(context.Context, uuid.UUID) (*store.LLMProviderData, error) {
	return nil, nil
}
func (s *embeddingProviderStoreStub) GetProviderByName(context.Context, string) (*store.LLMProviderData, error) {
	return nil, nil
}
func (s *embeddingProviderStoreStub) ListProviders(ctx context.Context) ([]store.LLMProviderData, error) {
	s.listTenant = store.TenantIDFromContext(ctx)
	return s.masterProviders, nil
}
func (s *embeddingProviderStoreStub) ListAllProviders(context.Context) ([]store.LLMProviderData, error) {
	s.listAllCalled = true
	return s.allProviders, nil
}
func (s *embeddingProviderStoreStub) UpdateProvider(context.Context, uuid.UUID, map[string]any) error {
	return nil
}
func (s *embeddingProviderStoreStub) DeleteProvider(context.Context, uuid.UUID) error { return nil }

func TestResolveEmbeddingProviderAutoDetectUsesMasterTenantOnly(t *testing.T) {
	embeddingSettings := json.RawMessage(`{"embedding":{"enabled":true}}`)
	providerStore := &embeddingProviderStoreStub{
		masterProviders: []store.LLMProviderData{{
			TenantID:     store.MasterTenantID,
			Name:         "master-embedding",
			ProviderType: store.ProviderOpenAICompat,
			APIBase:      "http://master.invalid/v1",
			APIKey:       "master-key",
			Enabled:      true,
			Settings:     embeddingSettings,
		}},
		allProviders: []store.LLMProviderData{{
			TenantID:     uuid.New(),
			Name:         "tenant-controlled-endpoint",
			ProviderType: store.ProviderOpenAICompat,
			Enabled:      true,
			Settings:     embeddingSettings,
		}},
	}

	provider := resolveEmbeddingProvider(providerStore, nil, nil)
	if provider == nil || provider.Name() != "master-embedding" {
		t.Fatalf("resolveEmbeddingProvider() = %v, want master-embedding", provider)
	}
	if providerStore.listTenant != store.MasterTenantID {
		t.Fatalf("ListProviders tenant = %s, want master tenant", providerStore.listTenant)
	}
	if providerStore.listAllCalled {
		t.Fatal("ListAllProviders was called; cross-tenant auto-detect must stay disabled")
	}
}

// probeEmbedder is a memory.EmbeddingProvider whose answer width is decided by the test.
type probeEmbedder struct {
	name  string
	model string
	width int
	err   error
}

func (p probeEmbedder) Name() string  { return p.name }
func (p probeEmbedder) Model() string { return p.model }
func (p probeEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	if p.err != nil {
		return nil, p.err
	}
	return [][]float32{make([]float32, p.width)}, nil
}

func TestProbeEmbeddingWidth(t *testing.T) {
	matched := &embeddingSchemaStoreStub{width: 1024}
	tests := []struct {
		name    string
		stores  store.ProviderStore
		emb     memory.EmbeddingProvider
		wantErr string
		wantNil bool
	}{
		{
			name: "native width equals the column", stores: matched,
			emb: probeEmbedder{name: "siliconflow", model: "BAAI/bge-m3", width: 1024}, wantNil: true,
		},
		{
			name: "wider model names both fixes", stores: &embeddingSchemaStoreStub{width: 1536},
			emb:     probeEmbedder{name: "openai", model: "text-embedding-3-large", width: 3072},
			wantErr: "goclaw migrate embed-dims --to 3072",
		},
		{
			// A quota or network failure is not a width disagreement: the gateway still
			// serves everything that does not need embeddings.
			name: "probe failure warns instead of aborting", stores: matched,
			emb: probeEmbedder{name: "siliconflow", model: "m", err: errors.New("429 too many requests")}, wantNil: true,
		},
		{
			name: "backend without vector columns is skipped", stores: &embeddingProviderStoreStub{},
			emb: probeEmbedder{name: "siliconflow", model: "m", width: 1024}, wantNil: true,
		},
		{name: "no embedding provider is skipped", stores: matched, emb: nil, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := probeEmbeddingWidth(context.Background(), tt.stores, tt.emb)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("probeEmbeddingWidth() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("probeEmbeddingWidth() = nil, want a startup-blocking mismatch")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "set embedding.dimensions to 1536") {
				t.Fatalf("error = %q, want it to also offer the provider-side fix", err)
			}
		})
	}
}

// embeddingSchemaStoreStub is a provider store that also answers the schema-width
// question, which is what puts the startup gate on its enforcing path.
type embeddingSchemaStoreStub struct {
	embeddingProviderStoreStub
	width int
	err   error
}

func (s *embeddingSchemaStoreStub) EmbeddingSchemaDimensions(context.Context) (int, error) {
	return s.width, s.err
}

func embeddingDimsProvider(name string, dims int) store.LLMProviderData {
	return store.LLMProviderData{
		Name:         name,
		ProviderType: store.ProviderOpenAICompat,
		Enabled:      true,
		Settings:     json.RawMessage(`{"embedding":{"enabled":true,"model":"m","dimensions":` + strconv.Itoa(dims) + `}}`),
	}
}

func TestCheckEmbeddingSchemaCompat(t *testing.T) {
	mismatched := []store.LLMProviderData{embeddingDimsProvider("wide", 1024)}

	tests := []struct {
		name    string
		stores  store.ProviderStore
		wantErr string
	}{
		{
			// A backend with no vector columns has no width to disagree with.
			name:    "backend without vector columns is skipped",
			stores:  &embeddingProviderStoreStub{masterProviders: mismatched},
			wantErr: "",
		},
		{
			name:    "provider matching the column width passes",
			stores:  &embeddingSchemaStoreStub{width: 1024, embeddingProviderStoreStub: embeddingProviderStoreStub{masterProviders: mismatched}},
			wantErr: "",
		},
		{
			name:    "provider wider than the column aborts with the migrate command",
			stores:  &embeddingSchemaStoreStub{width: 1536, embeddingProviderStoreStub: embeddingProviderStoreStub{masterProviders: mismatched}},
			wantErr: "goclaw migrate embed-dims --to 1024",
		},
		{
			name:    "unreadable column width aborts",
			stores:  &embeddingSchemaStoreStub{width: 0, err: errors.New("memory_chunks.embedding not found")},
			wantErr: "goclaw migrate up",
		},
		{
			name:    "unindexable column width aborts",
			stores:  &embeddingSchemaStoreStub{width: 3072, err: errors.New("above pgvector's 2000-dimensional index ceiling")},
			wantErr: "pgvector cannot index",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkEmbeddingSchemaCompat(context.Background(), tt.stores)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkEmbeddingSchemaCompat() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkEmbeddingSchemaCompat() = nil, want an error that names the fix")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
