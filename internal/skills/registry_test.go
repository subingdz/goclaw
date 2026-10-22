package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestValidateBundlePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		bad  bool
	}{
		{"plain file", "SKILL.md", false},
		{"nested", "scripts/run.sh", false},
		{"unicode name", "references/说明.md", false},
		{"parent escape", "../evil", true},
		{"inner escape", "scripts/../../evil", true},
		{"absolute", "/etc/passwd", true},
		{"windows separator", "scripts\\run.sh", true},
		{"uncleaned dot slash", "./SKILL.md", true},
		{"uncleaned double slash", "a//b", true},
		{"macos artifact", "__MACOSX/._SKILL.md", true},
		{"resource fork", "scripts/._run.sh", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBundlePath(tt.path)
			if tt.bad && err == nil {
				t.Fatalf("validateBundlePath(%q) = nil, want refusal", tt.path)
			}
			if !tt.bad && err != nil {
				t.Fatalf("validateBundlePath(%q) error = %v", tt.path, err)
			}
		})
	}
}

func bundleWith(paths ...string) *RegistryBundle {
	b := &RegistryBundle{Slug: "demo"}
	for _, p := range paths {
		b.Files = append(b.Files, RegistryFile{Path: p, Content: []byte("x"), Mode: 0o644})
	}
	return b
}

func TestRegistryBundleValidate(t *testing.T) {
	tests := []struct {
		name    string
		bundle  *RegistryBundle
		wantErr string
	}{
		{"ok", bundleWith("SKILL.md", "scripts/run.sh"), ""},
		{"bad slug", func() *RegistryBundle { b := bundleWith("SKILL.md"); b.Slug = "Bad_Slug"; return b }(), "unsafe slug"},
		{"no files", &RegistryBundle{Slug: "demo"}, "empty bundle"},
		{"missing root skill.md", bundleWith("scripts/run.sh"), "exactly one root SKILL.md"},
		{"nested skill.md counts as extra", bundleWith("SKILL.md", "sub/SKILL.md", "sub/x.sh"), ""},
		{"two root skill.md", func() *RegistryBundle {
			b := bundleWith("SKILL.md")
			b.Files = append(b.Files, RegistryFile{Path: "SKILL.md", Content: []byte("y")})
			return b
		}(), "exactly one root SKILL.md"},
		{"empty skill.md", func() *RegistryBundle {
			b := bundleWith("scripts/run.sh")
			b.Files = append(b.Files, RegistryFile{Path: "SKILL.md"})
			return b
		}(), "SKILL.md is empty"},
		{"empty helper file is not a rejection", func() *RegistryBundle {
			b := bundleWith("SKILL.md")
			b.Files = append(b.Files, RegistryFile{Path: "scripts/pkg/__init__.py"})
			return b
		}(), ""},
		{"traversal rejected", bundleWith("SKILL.md", "../out.sh"), "escapes the skill directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.bundle.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, tt.wantErr)
			}
			if !errors.Is(err, ErrUnsafeBundle) {
				t.Fatalf("error %v must wrap ErrUnsafeBundle", err)
			}
		})
	}
}

func TestRegistryBundleFileAndTotalCaps(t *testing.T) {
	b := bundleWith("SKILL.md")
	b.Files = append(b.Files, RegistryFile{Path: "big.bin", Content: bytes.Repeat([]byte("a"), registryMaxFileBytes+1)})
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "per-file cap") {
		t.Fatalf("oversized file error = %v, want the per-file cap", err)
	}

	many := &RegistryBundle{Slug: "many"}
	for i := 0; i < registryMaxFiles+1; i++ {
		many.Files = append(many.Files, RegistryFile{Path: "SKILL.md", Content: []byte("x")})
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "files exceeds") {
		t.Fatalf("file count error = %v, want the count cap", err)
	}
}

// zipEntry is one file to place in a test archive.
type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("create entry: %v", err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("write entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestFilesFromZipAppliesUploadGradeGuards(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "demo-skill/SKILL.md", body: "---\nname: Demo\ndescription: does things\n---\nbody\n"},
		zipEntry{name: "demo-skill/scripts/run.sh", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		zipEntry{name: "demo-skill/__MACOSX/._SKILL.md", body: "junk"},
		zipEntry{name: "../escape.txt", body: "no"},
		zipEntry{name: "demo-skill/", body: ""},
	)

	files, err := filesFromZip(raw)
	if err != nil {
		t.Fatalf("filesFromZip() error = %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want SKILL.md and the script only", archiveFileNames(files))
	}
	if files[0].Path != "SKILL.md" {
		t.Fatalf("wrapper directory not stripped: %v", archiveFileNames(files))
	}
	if files[1].Mode&0o111 == 0 {
		t.Fatalf("script lost its executable bit: %v", files[1].Mode)
	}
	for _, f := range files {
		if err := validateBundlePath(f.Path); err != nil {
			t.Fatalf("extracted path rejected by the validator: %v", err)
		}
	}
}

func archiveFileNames(files []RegistryFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func TestFilesFromZipRejectsTwoTopLevelDirs(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "a/SKILL.md", body: "x"},
		zipEntry{name: "b/SKILL.md", body: "y"},
	)
	files, err := filesFromZip(raw)
	if err != nil {
		t.Fatalf("filesFromZip() error = %v", err)
	}
	// No stripping means the bundle has no root SKILL.md, which Validate then refuses.
	if _, ok := findFile(files, "SKILL.md"); ok {
		t.Fatal("a two-top-level zip must not be flattened into a root SKILL.md")
	}
	b := &RegistryBundle{Slug: "demo", Files: files}
	if err := b.Validate(); err == nil {
		t.Fatal("Validate() accepted a bundle without a root SKILL.md")
	}
}

func findFile(files []RegistryFile, path string) (RegistryFile, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return RegistryFile{}, false
}

// --- ClawHub client, against a fake registry ---

const clawHubSkillMD = "---\nname: Real Name\ndescription: real description\nslug: real-slug\n---\ninstructions\n"

func newFakeClawHub(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClawHubFetch(t *testing.T) {
	archive := buildZip(t,
		zipEntry{name: "pkg/SKILL.md", body: clawHubSkillMD},
		zipEntry{name: "pkg/scripts/go.sh", body: "#!/bin/sh\n", mode: 0o755},
	)

	var gotAuth, gotQuery string
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/skills/pdf-tools":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"slug":"pdf-tools","displayName":"PDF Tools","summary":"index summary","latestVersion":{"version":"1.4.2"},"moderation":{"isMalwareBlocked":false,"isSuspicious":true}}`))
		case "/api/v1/download":
			gotAuth = r.Header.Get("Authorization")
			gotQuery = r.URL.Query().Encode()
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	})

	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv, Token: "tok-1"})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	bundle, err := reg.Fetch(context.Background(), "pdf-tools", "")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if gotAuth != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want the bearer token", gotAuth)
	}
	if !strings.Contains(gotQuery, "version=1.4.2") || !strings.Contains(gotQuery, "slug=pdf-tools") {
		t.Fatalf("download query = %q, want slug plus the version from the detail record", gotQuery)
	}
	if bundle.Version != "1.4.2" {
		t.Fatalf("version = %q, want the pinned registry version", bundle.Version)
	}
	// Identity comes from SKILL.md, not from the index row.
	if bundle.Name != "Real Name" || bundle.Description != "real description" || bundle.Slug != "real-slug" {
		t.Fatalf("identity = %+v, want the frontmatter values", bundle)
	}
	if !bundle.IsSuspicious() {
		t.Fatal("the registry's suspicious flag was swallowed")
	}
	tags := bundle.RegistryTags()
	if !strings.Contains(strings.Join(tags, " "), "hub-suspicious") || !strings.Contains(strings.Join(tags, " "), "source:clawhub") {
		t.Fatalf("tags = %v, want provenance plus the warning", tags)
	}
	if _, ok := findFile(bundle.Files, "SKILL.md"); !ok {
		t.Fatalf("files = %v, want the wrapper directory stripped", archiveFileNames(bundle.Files))
	}
}

func TestClawHubRefusesMalwareFlaggedSkill(t *testing.T) {
	var downloadCalled bool
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/download" {
			downloadCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slug":"bad","moderation":{"isMalwareBlocked":true,"isSuspicious":false}}`))
	})

	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	if _, err := reg.Fetch(context.Background(), "bad", ""); !errors.Is(err, ErrRegistryRefused) {
		t.Fatalf("Fetch() error = %v, want ErrRegistryRefused", err)
	}
	if downloadCalled {
		t.Fatal("a flagged skill's payload was still downloaded")
	}
}

func TestClawHubSearchParsesResults(t *testing.T) {
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("q") == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"score":0.9,"slug":"a","displayName":"A","summary":"sa","version":"2"},{"slug":""}]}`))
	})
	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}

	got, err := reg.Search(context.Background(), "weather", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].Slug != "a" || got[0].Version != "2" || got[0].Source != "clawhub" {
		t.Fatalf("results = %+v, want the one usable entry", got)
	}

	if _, err := reg.Search(context.Background(), "  ", 5); err == nil {
		t.Fatal("an empty query must be refused rather than treated as a full listing")
	}
}

func TestClawHubRejectsRedirectOffHost(t *testing.T) {
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/x", http.StatusFound)
	})
	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	if _, err := reg.Meta(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("Meta() error = %v, want the redirect refused", err)
	}
}

func TestNewClawHubRegistryValidation(t *testing.T) {
	if _, err := NewClawHubRegistry(ClawHubConfig{}); err == nil {
		t.Fatal("an empty base_url must be rejected")
	}
	if _, err := NewClawHubRegistry(ClawHubConfig{BaseURL: "https://clawhub.ai"}); err != nil {
		t.Fatalf("a normal https base was rejected: %v", err)
	}
	// Plain http is only for a local fake registry; a bearer token elsewhere would leak.
	if _, err := NewClawHubRegistry(ClawHubConfig{BaseURL: "http://registry.internal"}); err == nil {
		t.Fatal("plain http to a non-loopback host must be rejected")
	}
	if _, err := NewClawHubRegistry(ClawHubConfig{BaseURL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("loopback http should be allowed for tests: %v", err)
	}
}

func TestClawHubCapsResponse(t *testing.T) {
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf := bytes.Repeat([]byte("a"), registryMaxMetaBytes+1024)
		_, _ = w.Write(buf)
	})
	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	if _, err := reg.Meta(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("Meta() error = %v, want the size ceiling enforced before buffering", err)
	}
}

func TestClawHubNon2xxCarriesStatus(t *testing.T) {
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	if _, err := reg.Meta(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("Meta() error = %v, want the status code", err)
	}
}

// A malformed envelope must not become a nil dereference at 2 a.m. in an install flow.
func TestClawHubSearchRejectsGarbage(t *testing.T) {
	srv := newFakeClawHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": "not-a-list"})
	})
	reg, err := NewClawHubRegistry(ClawHubConfig{BaseURL: srv})
	if err != nil {
		t.Fatalf("NewClawHubRegistry() error = %v", err)
	}
	if _, err := reg.Search(context.Background(), "q", 5); err == nil {
		t.Fatal("a malformed search envelope was accepted")
	}
}
