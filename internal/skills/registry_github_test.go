package skills

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub serves just enough of the contents API to drive the walker, keyed by
// "path@ref". It records every requested path so a test can prove nothing was fetched twice
// and no download_url was ever followed.
type fakeGitHub struct {
	dirs     map[string][]gitContentEntry
	meta     map[string]gitRepoMeta
	requests []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/repos/nextlevelbuilder/skills":
		_ = json.NewEncoder(w).Encode(f.meta[""])
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		rest := strings.TrimPrefix(r.URL.Path, "/repos/nextlevelbuilder/skills/contents/")
		key := rest + "@" + r.URL.Query().Get("ref")
		entries, ok := f.dirs[key]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func fileEntry(path, body string) gitContentEntry {
	return gitContentEntry{
		Name:    path[strings.LastIndex(path, "/")+1:],
		Type:    "file",
		Path:    path,
		Size:    int64(len(body)),
		Content: base64.StdEncoding.EncodeToString([]byte(body)),
	}
}

func newGitRegistry(t *testing.T, f *fakeGitHub) *GitHubRegistry {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	reg, err := NewGitHubRegistry(GitHubConfig{APIBase: srv.URL})
	if err != nil {
		t.Fatalf("NewGitHubRegistry() error = %v", err)
	}
	return reg
}

func TestParseGitSlug(t *testing.T) {
	owner, repo, dir, err := parseGitSlug("nextlevelbuilder/goclaw/skills/pdf")
	if err != nil {
		t.Fatalf("parseGitSlug() error = %v", err)
	}
	if owner != "nextlevelbuilder" || repo != "goclaw" || dir != "skills/pdf" {
		t.Fatalf("parsed = %q/%q/%q", owner, repo, dir)
	}

	// A skill at the repo root needs no path part.
	if _, _, dir, err := parseGitSlug("a/b"); err != nil || dir != "" {
		t.Fatalf("root slug = %q, err %v", dir, err)
	}

	for _, bad := range []string{"", "solo", "../etc", "a/../../b", "a/b/../c", "a/b?x=y", "a/b#frag", "a/ b"} {
		if _, _, _, err := parseGitSlug(bad); err == nil {
			t.Fatalf("parseGitSlug(%q) accepted an unsafe slug", bad)
		}
	}
}

func TestGitHubFetchPutsSkillRootAtTheDeclaredDirectory(t *testing.T) {
	f := &fakeGitHub{
		meta: map[string]gitRepoMeta{"": {DefaultBranch: "main"}},
		dirs: map[string][]gitContentEntry{
			// The contents API answers one directory level at a time.
			"skills/pdf@main": {
				fileEntry("skills/pdf/SKILL.md", "---\nname: PDF Tools\ndescription: work with pdfs\n---\nread page 1\n"),
				{Name: "scripts", Type: "dir", Path: "skills/pdf/scripts"},
				{Name: ".github", Type: "dir", Path: "skills/pdf/.github"},
				{Name: "evil", Type: "symlink", Path: "skills/pdf/evil"},
			},
			"skills/pdf/scripts@main": {
				fileEntry("skills/pdf/scripts/extract.py", "print('hi')\n"),
			},
			"skills/pdf/.github@main": {
				fileEntry("skills/pdf/.github/workflow.yml", "name: deploy\n"),
			},
		},
	}
	reg := newGitRegistry(t, f)

	bundle, err := reg.Fetch(context.Background(), "nextlevelbuilder/skills/skills/pdf", "")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if bundle.Version != "main" {
		t.Fatalf("version = %q, want the repository's default branch", bundle.Version)
	}
	// Paths are relative to the skill directory, which is what the loader expects.
	got := archiveFileNames(bundle.Files)
	if len(got) != 2 || got[0] != "SKILL.md" || got[1] != "scripts/extract.py" {
		t.Fatalf("files = %v, want SKILL.md and scripts/extract.py", got)
	}
	if bundle.Name != "PDF Tools" || bundle.Description != "work with pdfs" {
		t.Fatalf("identity = %q/%q, want the frontmatter values", bundle.Name, bundle.Description)
	}
	if bundle.Files[1].Mode&0o111 == 0 {
		t.Fatal("scripts/extract.py lost its executable bit")
	}
	for _, req := range f.requests {
		if strings.Contains(req, ".github") {
			t.Fatalf("the walker entered a disallowed directory: %v", f.requests)
		}
	}
}

func TestGitHubFetchRefusesFileWithoutInlineContent(t *testing.T) {
	f := &fakeGitHub{
		meta: map[string]gitRepoMeta{"": {DefaultBranch: "trunk"}},
		dirs: map[string][]gitContentEntry{
			"@trunk": {
				{Name: "SKILL.md", Type: "file", Path: "SKILL.md", Size: 4 << 20, DownloadURL: "https://raw.githubusercontent.com/big"},
			},
		},
	}
	reg := newGitRegistry(t, f)

	_, err := reg.Fetch(context.Background(), "nextlevelbuilder/skills", "")
	if err == nil || !strings.Contains(err.Error(), "does not follow download") {
		t.Fatalf("Fetch() error = %v, want the inline-content refusal", err)
	}
}

func TestGitHubFetchNeedsRootSkillMD(t *testing.T) {
	f := &fakeGitHub{
		meta: map[string]gitRepoMeta{"": {DefaultBranch: "main"}},
		dirs: map[string][]gitContentEntry{
			"@main": {fileEntry("README.md", "just docs")},
		},
	}
	reg := newGitRegistry(t, f)

	if _, err := reg.Fetch(context.Background(), "nextlevelbuilder/skills", ""); err == nil {
		t.Fatal("a repository directory without SKILL.md must not install")
	}
}

func TestGitHubRateLimitIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	reg, err := NewGitHubRegistry(GitHubConfig{APIBase: srv.URL})
	if err != nil {
		t.Fatalf("NewGitHubRegistry() error = %v", err)
	}
	_, err = reg.Fetch(context.Background(), "a/b", "main")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("Fetch() error = %v, want the rate limit named", err)
	}
}

func TestNewGitHubRegistryValidation(t *testing.T) {
	if _, err := NewGitHubRegistry(GitHubConfig{APIBase: "http://ghe.internal"}); err == nil {
		t.Fatal("plain http to a non-loopback API base must be refused")
	}
	if _, err := NewGitHubRegistry(GitHubConfig{APIBase: "not a url"}); err == nil {
		t.Fatal("an unparseable API base must be refused")
	}
	reg, err := NewGitHubRegistry(GitHubConfig{})
	if err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if reg.api != githubDefaultAPI {
		t.Fatalf("api = %q, want %q", reg.api, githubDefaultAPI)
	}
}

func TestGitHubRejectsUnsafeRef(t *testing.T) {
	f := &fakeGitHub{meta: map[string]gitRepoMeta{"": {DefaultBranch: "main"}}}
	reg := newGitRegistry(t, f)

	if _, err := reg.Fetch(context.Background(), "nextlevelbuilder/skills", "../master"); err == nil {
		t.Fatal("a traversal ref must be refused before any request")
	}
	if len(f.requests) != 0 {
		t.Fatalf("requests = %v, want none", f.requests)
	}
}

func TestAllowedGitDirsAreTheDocumentedSet(t *testing.T) {
	for _, d := range []string{"scripts", "references", "assets", "templates", "docs"} {
		if !allowedGitDirs[d] {
			t.Fatalf("%s is missing from the allowed directories", d)
		}
	}
	if allowedGitDirs["node_modules"] || allowedGitDirs[".git"] {
		t.Fatal("an obvious non-skill directory was allowed")
	}
}

func TestInlineContentGuardsOversizedEntry(t *testing.T) {
	e := gitContentEntry{
		Path:    "big.bin",
		Size:    registryMaxFileBytes + 1,
		Content: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 64))),
	}
	if _, err := inlineContent(e); err == nil {
		t.Fatal("an oversized entry passed the inline limit")
	}
}

// A configured API base that redirects elsewhere must not be followed: the token is the same
// secret for every request, and content served from a different authority is not the content
// the operator agreed to talk to.
func TestGitHubRegistryPinsTheAPIHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRepoMeta{DefaultBranch: "main", FullName: "a/b"})
	}))
	defer other.Close()

	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer base.Close()

	reg, err := NewGitHubRegistry(GitHubConfig{APIBase: base.URL})
	if err != nil {
		t.Fatalf("NewGitHubRegistry() error = %v", err)
	}
	if _, err := reg.Fetch(context.Background(), "a/b", ""); err == nil {
		t.Fatal("a redirect off the pinned host was followed")
	} else if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("error = %v, want the redirect refusal to be named", err)
	}
}
