package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// ClawHub is the community skill registry the OpenClaw family uses. The endpoint shapes below
// are taken from a working client in that ecosystem rather than from published API docs, so
// they are written to be re-checked against a live deployment: `apiVersionPath` is the only
// place the version prefix lives.
type ClawHubRegistry struct {
	base   *url.URL
	token  string
	client *http.Client
	now    func() time.Time
}

// ClawHubConfig configures the client. BaseURL is required; Token is optional and only
// sent as a bearer header when set.
type ClawHubConfig struct {
	BaseURL string
	Token   string
	Timeout time.Duration
}

const (
	clawHubAPIPrefix   = "/api/v1"
	DefaultClawHubBase = "https://clawhub.ai"
)

// NewClawHubRegistry builds a client pinned to one host.
//
// The pin is the point: a registry response that redirects elsewhere would otherwise be a
// licence to make the gateway send its bearer token to any host the registry chooses.
func NewClawHubRegistry(cfg ClawHubConfig) (*ClawHubRegistry, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("clawhub: base_url is required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("clawhub: invalid base_url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("clawhub: base_url must be http(s), got %q", u.Scheme)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("clawhub: base_url must be https (a bearer token over plain http is a leak), got %q", u.Host)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = RegistryRequestBudget
	}

	pin := u.Host
	return &ClawHubRegistry{
		base:  u,
		token: cfg.Token,
		now:   time.Now,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if req.URL.Host != pin {
					return fmt.Errorf("clawhub: refusing redirect off %s to %s", pin, req.URL.Host)
				}
				return nil
			},
		},
	}, nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// Name identifies the source in config and in provenance tags.
func (r *ClawHubRegistry) Name() string { return "clawhub" }

type clawHubSearchResponse struct {
	Results []struct {
		Score       float64 `json:"score"`
		Slug        string  `json:"slug"`
		DisplayName string  `json:"displayName"`
		Summary     string  `json:"summary"`
		Version     string  `json:"version"`
	} `json:"results"`
}

// Search returns the registry's own ranking. Empty query is refused rather than treated as
// "list everything", because an unbounded listing is how a big registry becomes a download
// storm in a UI.
func (r *ClawHubRegistry) Search(ctx context.Context, query string, limit int) ([]RegistrySummary, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("clawhub: search needs a query")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	q := url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}}
	var payload clawHubSearchResponse
	if err := r.getJSON(ctx, "/search", q, &payload); err != nil {
		return nil, err
	}

	out := make([]RegistrySummary, 0, len(payload.Results))
	for _, res := range payload.Results {
		if res.Slug == "" {
			continue
		}
		out = append(out, RegistrySummary{
			Slug:        res.Slug,
			DisplayName: res.DisplayName,
			Summary:     res.Summary,
			Version:     res.Version,
			Score:       res.Score,
			Source:      r.Name(),
			URL:         r.webURL(res.Slug),
		})
	}
	return out, nil
}

type clawHubSkillResponse struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
	Summary     string `json:"summary"`
	Latest      struct {
		Version string `json:"version"`
	} `json:"latestVersion"`
	Moderation struct {
		IsMalwareBlocked bool `json:"isMalwareBlocked"`
		IsSuspicious     bool `json:"isSuspicious"`
	} `json:"moderation"`
}

// Meta reads the detail record, which is where the registry's safety verdict lives.
func (r *ClawHubRegistry) Meta(ctx context.Context, slug string) (RegistrySummary, error) {
	var payload clawHubSkillResponse
	if err := r.getJSON(ctx, "/skills/"+url.PathEscape(slug), nil, &payload); err != nil {
		return RegistrySummary{}, err
	}
	version := payload.Latest.Version
	return RegistrySummary{
		Slug:           payload.Slug,
		DisplayName:    payload.DisplayName,
		Summary:        payload.Summary,
		Version:        version,
		MalwareBlocked: payload.Moderation.IsMalwareBlocked,
		Suspicious:     payload.Moderation.IsSuspicious,
		Source:         r.Name(),
		URL:            r.webURL(slug),
	}, nil
}

// Fetch downloads one skill as a bundle. version=="" means the registry's latest.
func (r *ClawHubRegistry) Fetch(ctx context.Context, slug, version string) (*RegistryBundle, error) {
	if slug == "" {
		return nil, fmt.Errorf("clawhub: slug is required")
	}

	meta, err := r.Meta(ctx, slug)
	if err != nil {
		return nil, err
	}
	// Blocked is blocked: no flag in GoClaw's config overrides another party's malware
	// verdict, because the moment one does, the verdict is decorative.
	if meta.MalwareBlocked {
		return nil, fmt.Errorf("%w: %s/%s is flagged as malware by clawhub", ErrRegistryRefused, slug, version)
	}

	pinned := version
	if pinned == "" {
		pinned = meta.Version
	}

	q := url.Values{"slug": {slug}}
	if pinned != "" {
		q.Set("version", pinned)
	}
	raw, err := r.getBytes(ctx, "/download", q)
	if err != nil {
		return nil, err
	}

	files, err := filesFromZip(raw)
	if err != nil {
		return nil, fmt.Errorf("clawhub: %s: %w", slug, err)
	}

	bundle := &RegistryBundle{
		Slug:        slug,
		Name:        meta.DisplayName,
		Description: meta.Summary,
		Version:     pinned,
		Files:       files,
		Origin: RegistryOrigin{
			Source:      r.Name(),
			Slug:        slug,
			Version:     pinned,
			URL:         r.webURL(slug),
			RetrievedAt: r.now().UTC(),
		},
	}
	// Identity first, checks second: SKILL.md may carry a different slug than the index
	// claimed, and Validate must judge the values that will actually be written.
	if err := bundle.applyFrontmatterIdentity(); err != nil {
		return nil, err
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	bundle.markSuspicious(meta.Suspicious)
	return bundle, nil
}

func (r *ClawHubRegistry) webURL(slug string) string {
	return strings.TrimRight(r.base.String(), "/") + "/skills/" + slug
}

func (r *ClawHubRegistry) getJSON(ctx context.Context, apiPath string, q url.Values, out any) error {
	raw, err := r.getBytes(ctx, apiPath, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("clawhub: decode %s: %w", apiPath, err)
	}
	return nil
}

func (r *ClawHubRegistry) getBytes(ctx context.Context, apiPath string, q url.Values) ([]byte, error) {
	u := *r.base
	u.Path = strings.TrimRight(u.Path, "/") + clawHubAPIPrefix + apiPath
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("clawhub: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clawhub: %s: %w", apiPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("clawhub: %s http %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	limit := int64(registryMaxMetaBytes)
	if apiPath == "/download" {
		limit = registryMaxZipBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("clawhub: read %s: %w", apiPath, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("clawhub: %s exceeded the %d byte ceiling", apiPath, limit)
	}
	return raw, nil
}

// filesFromZip turns registry bytes into validated relative paths. Every guard the ZIP upload
// handler applies is applied here too — symlinks, traversal, wrapper directories, archive
// artifacts — because this is the same untrusted input from a different pipe.
//
// Entry names are normalized and judged in a first pass before anything is read, so a junk or
// hostile entry cannot cancel the wrapper-directory detection for the entries that are fine.
func filesFromZip(raw []byte) ([]RegistryFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("not a readable zip: %w", err)
	}

	var candidates []archiveEntry
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // never a target for a write
		}
		name, ok := normalizeEntryName(f.Name)
		if !ok {
			slog.Warn("skill registry: skipping unsafe archive entry", "entry", f.Name)
			continue
		}
		candidates = append(candidates, archiveEntry{name: name, file: f})
	}

	strip := wrapperDirPrefix(archiveNames(candidates))

	out := make([]RegistryFile, 0, len(candidates))
	for _, cand := range candidates {
		name := strings.TrimPrefix(cand.name, strip)
		if name == "" {
			continue
		}

		rc, err := cand.file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, registryMaxFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if int64(len(content)) > registryMaxFileBytes {
			return nil, fmt.Errorf("%s exceeds the per-file cap", name)
		}

		mode := cand.file.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out = append(out, RegistryFile{Path: name, Content: content, Mode: mode})
	}
	return out, nil
}

// normalizeEntryName turns an archive entry into a slash-separated relative path, rejecting
// anything that could escape the skill directory instead of letting it reach the writer.
func normalizeEntryName(raw string) (string, bool) {
	name := strings.ReplaceAll(strings.TrimPrefix(raw, "./"), "\\", "/")
	if name == "" || IsSystemArtifact(name) {
		return "", false
	}
	if err := validateBundlePath(path.Clean(name)); err != nil {
		return "", false
	}
	return path.Clean(name), true
}

// wrapperDirPrefix returns "topdir/" when every surviving entry shares one top directory,
// which is how a registry archive of a single skill usually looks.
func wrapperDirPrefix(names []string) string {
	strip := ""
	for _, name := range names {
		i := strings.Index(name, "/")
		if i <= 0 {
			return "" // files at the root: nothing to strip
		}
		head := name[:i+1]
		if strip == "" {
			strip = head
		} else if strip != head {
			return ""
		}
	}
	return strip
}

var (
	_ Registry = (*ClawHubRegistry)(nil)
	_ Searcher = (*ClawHubRegistry)(nil)
)
