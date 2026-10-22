package skills

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Install refusal sentinels. They are separate because a caller needs to tell "this bundle is
// dangerous" from "this slug already belongs to something else": the first is a hard no, the
// second is a naming decision the operator can resolve.
var (
	// ErrInstallRefused is returned when the content guard finds a reason not to install.
	ErrInstallRefused = errors.New("skills: install refused")

	// ErrInstallConflict is returned when the target slug is a system skill or is owned by
	// another user in the tenant, so installing would replace something the caller does not own.
	ErrInstallConflict = errors.New("skills: install conflicts with an existing skill")
)

// FileAnalysis describes one file of a candidate bundle without carrying its contents.
type FileAnalysis struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Mode  string `json:"mode"`
}

// Analysis is what a human is shown before agreeing to install a community skill, and what an
// install records alongside the files.
//
// Findings carry the guard's reasons and line numbers, never its regex sources: publishing the
// pattern that matched teaches a would-be publisher how to write content that passes.
type Analysis struct {
	Source      string `json:"source"`
	Locator     string `json:"locator"`
	URL         string `json:"url,omitempty"`
	Version     string `json:"version,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	Files       []FileAnalysis `json:"files"`
	TotalBytes  int64          `json:"total_bytes"`
	Executables []string       `json:"executables,omitempty"`

	// Declared is the skill's own dependency list (pip:/npm:/system:/python:/node:), and
	// MissingDeps is the subset this host cannot satisfy right now. A reviewer needs both:
	// one says what the skill wants, the other says what installing it will actually do.
	Declared    []string `json:"declared,omitempty"`
	MissingDeps []string `json:"missing_deps,omitempty"`
	NeedsShell  bool     `json:"needs_shell,omitempty"`

	Findings   []string  `json:"findings,omitempty"`
	Unsafe     bool      `json:"unsafe"`
	Suspicious bool      `json:"suspicious"`
	CheckedAt  time.Time `json:"checked_at"`
}

// checkBundleDeps is a seam for tests: dependency probing looks at host binaries and npm
// globals, so an install test must not depend on what machine runs it.
var checkBundleDeps = CheckSkillDeps

// AnalyzeBundle inspects a downloaded bundle without keeping it anywhere. The dependency
// scanner reads a directory, so the bundle is staged in a temp dir that is removed before
// returning — inspecting must never leave a half-trusted skill on disk.
func AnalyzeBundle(b *RegistryBundle) (Analysis, error) {
	if b == nil {
		return Analysis{}, errors.New("skills: nil bundle")
	}
	if err := b.Validate(); err != nil {
		return Analysis{}, err
	}

	dir, err := os.MkdirTemp("", "goclaw-skill-inspect-*")
	if err != nil {
		return Analysis{}, fmt.Errorf("skills: stage bundle: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("skills: could not remove inspection directory", "path", dir, "error", err)
		}
	}()
	if err := writeBundle(dir, b); err != nil {
		return Analysis{}, err
	}

	a := Analysis{
		Source:     b.Origin.Source,
		Locator:    b.Origin.Slug,
		URL:        b.Origin.URL,
		Version:    b.Version,
		Name:       b.Name,
		Suspicious: b.IsSuspicious(),
		CheckedAt:  time.Now().UTC(),
	}
	if b.Description != "" {
		a.Description = b.Description
	}

	for _, f := range b.Files {
		a.Files = append(a.Files, FileAnalysis{Path: f.Path, Bytes: len(f.Content), Mode: f.Mode.String()})
		a.TotalBytes += int64(len(f.Content))
		if f.Mode&0o111 != 0 {
			a.Executables = append(a.Executables, f.Path)
		}
	}
	sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Path < a.Files[j].Path })
	sort.Strings(a.Executables)

	if manifest := ScanSkillDeps(dir); manifest != nil && !manifest.IsEmpty() {
		a.Declared = append(a.Declared, manifest.Explicit...)
		a.Declared = append(a.Declared, manifest.Requires...)
		if len(manifest.RequiresPython) > 0 {
			a.Declared = append(a.Declared, "python:"+strings.Join(manifest.RequiresPython, ","))
		}
		if len(manifest.RequiresNode) > 0 {
			a.Declared = append(a.Declared, "node:"+strings.Join(manifest.RequiresNode, ","))
		}
		if ok, missing := checkBundleDeps(manifest); !ok {
			a.MissingDeps = missing
		}
	}

	for _, f := range b.Files {
		if !isPlainTextSkillFile(f.Path) {
			continue
		}
		violations, safe := GuardSkillContent(string(f.Content))
		if safe {
			continue
		}
		a.Unsafe = true
		for _, v := range violations {
			a.Findings = append(a.Findings, fmt.Sprintf("%s:%d: %s", f.Path, v.Line, v.Reason))
		}
	}
	a.NeedsShell = looksLikeShellUse(b)
	return a, nil
}

// isPlainTextSkillFile limits the guard to files that can carry instructions an agent would
// follow. Binary assets are not scanned for prose patterns they cannot contain.
func isPlainTextSkillFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".txt", ".sh", ".bash", ".py", ".js", ".ts", ".json", ".yaml", ".yml", ".toml", "":
		return true
	}
	return false
}

func looksLikeShellUse(b *RegistryBundle) bool {
	for _, f := range b.Files {
		if firstSegment(f.Path) == "scripts" {
			return true
		}
		switch strings.ToLower(filepath.Ext(f.Path)) {
		case ".sh", ".bash":
			return true
		}
	}
	return false
}

// OriginFileName is the sidecar that records where an installed skill came from. It lives with
// the files rather than only in the database because the directory is what export, import and a
// manual audit all see.
const OriginFileName = ".skill-origin.json"

// writeBundle places a validated bundle under destDir. Paths were checked by Validate, and they
// are checked again here because writing is the point where a mistake stops being theoretical.
func writeBundle(destDir string, b *RegistryBundle) error {
	for _, f := range b.Files {
		if err := validateBundlePath(f.Path); err != nil {
			return err
		}
		full := filepath.Join(destDir, filepath.FromSlash(f.Path))
		if !strings.HasPrefix(full, destDir+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s resolves outside %s", ErrUnsafeBundle, f.Path, destDir)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("skills: create directory: %w", err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return fmt.Errorf("skills: write %s: %w", f.Path, err)
		}
	}

	origin := b.Origin
	origin.RetrievedAt = origin.RetrievedAt.UTC()
	data, err := json.MarshalIndent(origin, "", "  ")
	if err != nil {
		return fmt.Errorf("skills: encode origin: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, OriginFileName), data, 0o644); err != nil {
		return fmt.Errorf("skills: write origin: %w", err)
	}
	return nil
}

// registrySkillStore is the part of the skill store an install needs. It is narrower than
// store.SkillManageStore on purpose: an installer that can also revoke grants, toggle skills or
// reach into the FS columns has more power than "write one directory and register it".
type registrySkillStore interface {
	GetSkill(ctx context.Context, name string) (*store.SkillInfo, bool)
	IsSystemSkill(slug string) bool
	GetSkillHashBySlug(ctx context.Context, slug string) (hash string, version int, ok bool)
	GetNextVersion(ctx context.Context, slug string) int
	CreateSkillManaged(ctx context.Context, p store.SkillCreateParams) (uuid.UUID, error)
}

// RegistryInstaller turns a fetched bundle into a managed skill in a tenant's skills-store.
//
// It writes no grants and installs no dependencies: installing and trusting are two decisions,
// and running somebody else's setup script is a third. A skill that arrives from a registry is
// active-but-ungranted, or archived until its declared deps exist on this host.
type RegistryInstaller struct {
	skills registrySkillStore

	// locks serialise version allocation per (directory, slug), mirroring the upload path.
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRegistryInstaller(skills registrySkillStore) *RegistryInstaller {
	return &RegistryInstaller{skills: skills, locks: map[string]*sync.Mutex{}}
}

// RegistryInstall is what a caller reports back to the operator.
type RegistryInstall struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Version   int       `json:"version"`
	Dir       string    `json:"dir"`
	Status    string    `json:"status"`
	Missing   []string  `json:"missing_deps,omitempty"`
	Unchanged bool      `json:"unchanged"`
	Analysis  Analysis  `json:"analysis"`
}

// InstallBundle writes one bundle under tenantStoreDir/slug/version/ and registers it.
//
// Nothing is visible to the loader until the database row exists, so a failure at any step
// leaves the previous state untouched rather than half a skill. tenant scope comes from ctx:
// every store call below is tenant-filtered, so a caller that forgets to set it writes into the
// master tenant rather than crossing into another one.
func (in *RegistryInstaller) InstallBundle(ctx context.Context, tenantStoreDir string, b *RegistryBundle, ownerID string) (*RegistryInstall, error) {
	if in == nil || in.skills == nil {
		return nil, errors.New("skills: installer has no store")
	}
	if b == nil {
		return nil, errors.New("skills: nil bundle")
	}
	if strings.TrimSpace(tenantStoreDir) == "" {
		return nil, errors.New("skills: install needs a tenant skills directory")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}

	unlock := in.lock(tenantStoreDir + "|" + b.Slug)
	defer unlock()

	analysis, err := AnalyzeBundle(b)
	if err != nil {
		return nil, err
	}
	if analysis.Unsafe {
		return nil, fmt.Errorf("%w: %s has %d unsafe finding(s): %s",
			ErrInstallRefused, b.Slug, len(analysis.Findings), strings.Join(analysis.Findings, "; "))
	}

	// The skills table is keyed by (tenant, slug) and CreateSkillManaged upserts, so an
	// existing row is not "a previous version of this install" by default: it can be another
	// user's skill, or a system skill. Overwriting either silently is a takeover.
	existing, found := in.skills.GetSkill(ctx, b.Slug)
	if found && existing.Slug == b.Slug {
		if existing.IsSystem || in.skills.IsSystemSkill(b.Slug) {
			return nil, fmt.Errorf("%w: %s is a system skill; a registry bundle cannot replace it", ErrInstallConflict, b.Slug)
		}
		if existing.OwnerID != "" && existing.OwnerID != ownerID {
			return nil, fmt.Errorf("%w: slug %s is owned by another user in this tenant, who must rename or delete it first", ErrInstallConflict, b.Slug)
		}
	}

	hash := skillMDHash(b)
	if prevHash, prevVer, ok := in.skills.GetSkillHashBySlug(ctx, b.Slug); ok && prevHash != "" && prevHash == hash {
		res := &RegistryInstall{
			Slug:      b.Slug,
			Version:   prevVer,
			Dir:       filepath.Join(tenantStoreDir, b.Slug, fmt.Sprintf("%d", prevVer)),
			Status:    "active",
			Unchanged: true,
			Analysis:  analysis,
		}
		if found && existing.Slug == b.Slug {
			if id, err := uuid.Parse(existing.ID); err == nil {
				res.ID = id
			}
			if existing.BaseDir != "" {
				res.Dir = existing.BaseDir
			}
			res.Status = existing.Status
			res.Missing = existing.MissingDeps
		}
		return res, nil
	}

	version := in.skills.GetNextVersion(ctx, b.Slug)
	destDir := filepath.Join(tenantStoreDir, b.Slug, fmt.Sprintf("%d", version))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("skills: create %s: %w", destDir, err)
	}
	if err := writeBundle(destDir, b); err != nil {
		os.RemoveAll(destDir)
		return nil, err
	}

	status := "active"
	var missing []string
	if len(analysis.MissingDeps) > 0 {
		status = "archived"
		missing = analysis.MissingDeps
	}

	description := b.Description
	params := store.SkillCreateParams{
		Name:        b.Name,
		Slug:        b.Slug,
		Description: &description,
		OwnerID:     ownerID,
		Visibility:  "internal",
		Status:      status,
		MissingDeps: missing,
		Version:     version,
		FilePath:    destDir,
		FileSize:    analysis.TotalBytes,
		FileHash:    &hash,
		Frontmatter: registryFrontmatter(b, hash, analysis),
	}
	id, err := in.skills.CreateSkillManaged(ctx, params)
	if err != nil {
		os.RemoveAll(destDir)
		return nil, fmt.Errorf("skills: register %s: %w", b.Slug, err)
	}

	slog.Info("skill installed from registry",
		"id", id, "slug", b.Slug, "version", version,
		"source", b.Origin.Source, "locator", b.Origin.Slug,
		"status", status, "files", len(b.Files), "bytes", analysis.TotalBytes)

	return &RegistryInstall{
		ID: id, Slug: b.Slug, Version: version, Dir: destDir,
		Status: status, Missing: missing, Analysis: analysis,
	}, nil
}

// registryFrontmatter copies the provenance into the database row. The skill's own frontmatter
// keys are preserved, and the registry fields are namespaced so a later rescan of SKILL.md
// cannot look like it changed where the skill came from.
func registryFrontmatter(b *RegistryBundle, hash string, a Analysis) map[string]string {
	fields := make(map[string]string, len(b.Frontmatter)+7)
	for k, v := range b.Frontmatter {
		fields[k] = v
	}
	fields["registry_source"] = b.Origin.Source
	fields["registry_locator"] = b.Origin.Slug
	fields["registry_version"] = b.Version
	fields["registry_sha256"] = hash
	if b.Origin.URL != "" {
		fields["registry_url"] = b.Origin.URL
	}
	if b.IsSuspicious() {
		fields["registry_suspicious"] = "true"
	}
	if len(a.Findings) > 0 {
		fields["registry_findings"] = strings.Join(a.Findings, " | ")
	}
	return fields
}

func (in *RegistryInstaller) lock(key string) func() {
	in.mu.Lock()
	m, ok := in.locks[key]
	if !ok {
		m = &sync.Mutex{}
		in.locks[key] = m
	}
	in.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ReadStoredOrigin returns the sidecar written at install time, if the directory has one. The UI
// uses it to label an installed community skill without a database query per row.
func ReadStoredOrigin(skillDir string) (RegistryOrigin, bool) {
	var origin RegistryOrigin
	data, err := os.ReadFile(filepath.Join(skillDir, OriginFileName))
	if err != nil {
		return origin, false
	}
	if err := json.Unmarshal(data, &origin); err != nil || origin.Source == "" {
		return origin, false
	}
	return origin, true
}

// skillMDHash hashes SKILL.md rather than the bundle, matching the upload path so a re-install
// of content-identical files reports "unchanged" instead of creating another version.
func skillMDHash(b *RegistryBundle) string {
	for _, f := range b.Files {
		if f.Path == "SKILL.md" {
			return fmt.Sprintf("%x", sha256.Sum256(f.Content))
		}
	}
	return ""
}
