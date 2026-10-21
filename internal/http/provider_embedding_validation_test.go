package http

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func embeddingProvider(dims int) *store.LLMProviderData {
	settings := map[string]any{
		"embedding": map[string]any{
			"enabled":    true,
			"model":      "test-model",
			"dimensions": dims,
		},
	}
	raw, _ := json.Marshal(settings)
	return &store.LLMProviderData{Settings: raw}
}

func TestValidateProviderEmbeddingSettings(t *testing.T) {
	tests := []struct {
		name       string
		p          *store.LLMProviderData
		schemaDims int
		wantErr    bool
	}{
		{"nil settings", &store.LLMProviderData{}, 1536, false},
		{"embedding disabled", &store.LLMProviderData{
			Settings: json.RawMessage(`{"embedding":{"enabled":false}}`),
		}, 1536, false},
		{"dimensions omitted (0)", &store.LLMProviderData{
			Settings: json.RawMessage(`{"embedding":{"enabled":true,"model":"m"}}`),
		}, 1536, false},
		{"matches the schema width", embeddingProvider(1024), 1024, false},
		{"disagrees with the schema width", embeddingProvider(1024), 1536, true},
		{
			// SQLite/desktop backends have no vector columns, so there is no width to
			// match — but a width pgvector could never index is still refused.
			name:       "backend without vector columns accepts an indexable width",
			p:          embeddingProvider(1024),
			schemaDims: 0,
			wantErr:    false,
		},
		{"above pgvector index ceiling", embeddingProvider(2048), 2048, true},
		{"above pgvector index ceiling without a schema width", embeddingProvider(2048), 0, true},
		{"dimensions negative", embeddingProvider(-1), 1536, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProviderEmbeddingSettings(tt.p, tt.schemaDims)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateProviderEmbeddingSettings() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateProviderEmbeddingSettingsOffersBothFixes(t *testing.T) {
	err := validateProviderEmbeddingSettings(embeddingProvider(1024), 1536)
	if err == nil {
		t.Fatal("validateProviderEmbeddingSettings() = nil, want a rejection")
	}
	msg := err.Error()
	for _, want := range []string{"vector(1536)", "goclaw migrate embed-dims --to 1024"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to mention %q", msg, want)
		}
	}
}
