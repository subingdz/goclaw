package skills

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// GitHubRegistry installs a skill straight out of a repository, which is the other half of how
// community skills actually circulate: plenty are published as a repo directory long before
// (or instead of) appearing in a registry.
//
// It has no Search — see the Registry interface note. It has one capability worth stating
// plainly: it reads only `file` entries whose base64 content GitHub returns inline, never a
// `download_url`, because that URL is data in a response and fetching data-supplied URLs is how
// a metadata endpoint gets probed from inside your network.
type GitHubRegistry struct {
	token  string
	api    string // https://api.github.com, overridable for a GHES install
	client *http.Client
	now    func() time.Time
}

type GitHubConfig struct {
	Token   string
	APIBase string
	Timeout time.Duration
}

const (
	githubDefaultAPI   = "https://api.github.com"
	githubMaxTreeDepth = 3
	// githubInlineLimit is GitHub's own ceiling for base64 `content` in the contents API.
	// Anything larger would need a download_url, which this client refuses to follow.
	githubInlineLimit = 1 << 20
)

// allowedGitDirs bounds what a repository may contribute to a skill. Everything else in a
// repo — .github workflows, CI configs, docs sites — is not skill content.
var allowedGitDirs = map[string]bool{
	"scripts": true, "references": true, "assets": true, "templates": true, "docs": true,
}

func NewGitHubRegistry(cfg GitHubConfig) (*GitHubRegistry, error) {
	api := strings.TrimRight(cfg.APIBase, "/")
	if api == "" {
		api = githubDefaultAPI
	}
	u, err := url.Parse(api)
	if err != nil {
		return nil, fmt.Errorf("github: invalid api base: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("github: api base must be https, got %q", api)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = RegistryRequestBudget
	}
	pin := u.Host
	return &GitHubRegistry{
		token: cfg.Token,
		api:   api,
		now:   time.Now,
		client: &http.Client{
			Timeout: timeout,
			// Go strips Authorization across hosts, but pinning here also refuses a base
			// that redirects content elsewhere — the same rule the ClawHub client applies.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if req.URL.Host != pin {
					return fmt.Errorf("github: refusing redirect off %s to %s", pin, req.URL.Host)
				}
				return nil
			},
		},
	}, nil
}

// Name identifies the source in config and provenance tags.
func (r *GitHubRegistry) Name() string { return "github" }

type gitContentEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // file | dir | symlink | submodule
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
	SHA         string `json:"sha"`
}

type gitRepoMeta struct {
	DefaultBranch string `json:"default_branch"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
}

// Fetch walks owner/repo[/path] and returns the directory as a bundle.
func (r *GitHubRegistry) Fetch(ctx context.Context, slug, ref string) (*RegistryBundle, error) {
	owner, repo, dir, err := parseGitSlug(slug)
	if err != nil {
		return nil, err
	}
	if err := checkGitSegment(ref); err != nil {
		return nil, fmt.Errorf("github: invalid ref: %w", err)
	}

	if ref == "" {
		var meta gitRepoMeta
		if err := r.getJSON(ctx, "/repos/"+owner+"/"+repo, nil, &meta); err != nil {
			return nil, err
		}
		ref = meta.DefaultBranch
		if ref == "" {
			return nil, fmt.Errorf("github: %s/%s has no default branch to read", owner, repo)
		}
	}

	var files []RegistryFile
	if err := r.walk(ctx, owner, repo, ref, dir, "", 1, &files); err != nil {
		return nil, err
	}

	slugPath := strings.Trim(strings.Join([]string{owner, repo, dir}, "/"), "/")
	bundle := &RegistryBundle{
		// Slug stays empty on purpose: the bundle's slug is the skill's own name, decided
		// below from SKILL.md. The repository locator belongs in Origin, where it cannot be
		// mistaken for an install path.
		Version: ref,
		Files:   files,
		Origin: RegistryOrigin{
			Source:      r.Name(),
			Slug:        slugPath,
			Version:     ref,
			URL:         "https://github.com/" + slugPath + "/tree/" + url.PathEscape(ref),
			RetrievedAt: r.now().UTC(),
		},
	}
	if err := bundle.applyFrontmatterIdentity(); err != nil {
		return nil, err
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// walk lists one directory in the repository and appends what belongs to the skill.
// apiPath is where to ask GitHub; relPrefix is what the fetched files are named relative to,
// so a skill living in skills/pdf/ installs as SKILL.md and not skills/pdf/SKILL.md.
func (r *GitHubRegistry) walk(ctx context.Context, owner, repo, ref, apiPath, relPrefix string, depth int, out *[]RegistryFile) error {
	if depth > githubMaxTreeDepth {
		return nil // a deeper tree is somebody else's project layout, not a skill
	}
	listPath := "/repos/" + owner + "/" + repo + "/contents/" + strings.TrimPrefix(apiPath, "/")
	q := url.Values{"ref": {ref}}

	var entries []gitContentEntry
	if err := r.getJSON(ctx, listPath, q, &entries); err != nil {
		return err
	}

	for _, e := range entries {
		rel := e.Name
		childPath := e.Path
		if relPrefix != "" {
			rel = relPrefix + "/" + e.Name
		}
		switch e.Type {
		case "file":
			// Only files at the skill root and inside the directories a skill may use.
			// Everything else in a repository — CI config, site sources, vendored code — is
			// somebody else's project layout, not part of the skill.
			if relPrefix != "" && !allowedGitDirs[firstSegment(relPrefix)] {
				continue
			}
			content, err := inlineContent(e)
			if err != nil {
				return err
			}
			name, ok := normalizeEntryName(rel)
			if !ok {
				slog.Warn("github: skipping unsafe path", "path", rel)
				continue
			}
			mode := os.FileMode(0o644)
			if looksLikeScript(name) {
				mode = 0o755
			}
			*out = append(*out, RegistryFile{Path: name, Content: content, Mode: mode})

		case "dir":
			if !allowedGitDirs[e.Name] {
				continue
			}
			if err := r.walk(ctx, owner, repo, ref, childPath, rel, depth+1, out); err != nil {
				return err
			}

		default:
			// symlink and submodule entries can point anywhere off-repo; never followed.
			slog.Debug("github: skipping non-file entry", "path", rel, "type", e.Type)
		}
	}
	return nil
}

func inlineContent(e gitContentEntry) ([]byte, error) {
	if e.Content == "" {
		return nil, fmt.Errorf("github: %s has no inline content (size %d); this client does not follow download URLs", e.Path, e.Size)
	}
	if e.Size > githubInlineLimit || len(e.Content) > 4*githubInlineLimit {
		return nil, fmt.Errorf("github: %s is larger than the %d byte inline limit and this client does not follow download URLs",
			e.Path, githubInlineLimit)
	}
	// GitHub returns base64 with embedded newlines.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(e.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decode %s: %w", e.Path, err)
	}
	if int64(len(raw)) > registryMaxFileBytes {
		return nil, fmt.Errorf("github: %s exceeds the per-file cap", e.Path)
	}
	return raw, nil
}

// parseGitSlug accepts owner/repo and owner/repo/path/to/skill.
func parseGitSlug(slug string) (owner, repo, dir string, err error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(slug), "/"), "/")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf(`github: expected "owner/repo" or "owner/repo/path", got %q`, slug)
	}
	owner, repo = parts[0], parts[1]
	if err := checkGitSegment(owner); err != nil {
		return "", "", "", fmt.Errorf("github: owner: %w", err)
	}
	if err := checkGitSegment(repo); err != nil {
		return "", "", "", fmt.Errorf("github: repo: %w", err)
	}
	dir = strings.Join(parts[2:], "/")
	// The directory is interpolated into an API path, so it gets the same strictness.
	for _, seg := range parts[2:] {
		if err := checkGitSegment(seg); err != nil {
			return "", "", "", fmt.Errorf("github: path segment %q: %w", seg, err)
		}
	}
	return owner, repo, dir, nil
}

// checkGitSegment allows only what GitHub itself accepts in a repo, ref, or path segment and
// nothing that could reshape the URL: no dots alone, no separators, no whitespace, no control
// bytes.
func checkGitSegment(s string) error {
	if s == "" {
		return nil // empty means "unset", handled by callers
	}
	if s == "." || s == ".." || strings.ContainsAny(s, "/\\?#% \t\x00") {
		return fmt.Errorf("unsafe segment %q", s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("segment %q contains control characters", s)
		}
	}
	return nil
}

func firstSegment(p string) string {
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

func looksLikeScript(name string) bool {
	switch firstSegment(name) {
	case "scripts":
		return true
	}
	return strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".bash") ||
		strings.HasSuffix(name, ".py") || strings.HasSuffix(name, ".js")
}

func (r *GitHubRegistry) getJSON(ctx context.Context, apiPath string, q url.Values, out any) error {
	u := r.api + apiPath
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", apiPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Unauthenticated GitHub answers 403/429 on rate limits, which reads like a
		// permission problem unless the status is stated.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
				if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
					if sec, convErr := strconv.ParseInt(reset, 10, 64); convErr == nil {
						return fmt.Errorf("github: rate limited until %s: %s",
							time.Unix(sec, 0).UTC().Format(time.RFC3339), strings.TrimSpace(string(body)))
					}
				}
				return fmt.Errorf("github: rate limited: %s", strings.TrimSpace(string(body)))
			}
		}
		return fmt.Errorf("github: %s http %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxMetaBytes+1))
	if err != nil {
		return fmt.Errorf("github: read %s: %w", apiPath, err)
	}
	if int64(len(raw)) > registryMaxMetaBytes {
		return fmt.Errorf("github: %s exceeded the %d byte ceiling", apiPath, registryMaxMetaBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", apiPath, err)
	}
	return nil
}

var _ Registry = (*GitHubRegistry)(nil)
