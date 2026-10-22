package skills

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// Community skill sources. GoClaw had no way to fetch a skill from anywhere but disk, a ZIP
// upload, or a seed: everything below exists so that a hub download ends up going through the
// same checks an upload goes through, rather than a second, weaker path.
const (
	// Ceilings on a downloaded bundle. A skill is text plus a few helper scripts; anything
	// approaching these numbers is either a mistake or something to refuse, not to install.
	registryMaxBundleBytes = 20 << 20
	registryMaxFileBytes   = 5 << 20
	registryMaxFiles       = 200

	// Ceilings on the wire, separate from the bundle: metadata answers are small JSON, and a
	// registry that streams forever is a download we should stop rather than buffer.
	registryMaxMetaBytes = 2 << 20
	registryMaxZipBytes  = 50 << 20

	// RegistryRequestBudget is the whole budget for one registry round trip: the default client
	// timeout here, and the deadline an HTTP handler puts around search, inspect and install.
	RegistryRequestBudget = 45 * time.Second
)

var (
	// ErrRegistryRefused is returned when a source says the content is unsafe. It is not
	// retried and not overridden by configuration: the point of a moderation flag is that
	// somebody looked.
	ErrRegistryRefused = errors.New("skill registry: content refused by the source's moderation")

	// ErrUnsafeBundle is returned when a downloaded bundle carries a path or shape that a
	// skill must never have.
	ErrUnsafeBundle = errors.New("skill registry: bundle failed path or size checks")

	// ErrNoRegistries is returned when the feature is used but no source is enabled.
	ErrNoRegistries = errors.New("skill registry: no community sources are enabled")

	// ErrRegistryNotSearchable is returned when a source that only installs by name is asked
	// for a keyword search.
	ErrRegistryNotSearchable = errors.New("skill registry: source has no search; install it by owner/repo/path")

	// ErrAllRegistriesFailed distinguishes "every source errored" from "nothing matched".
	ErrAllRegistriesFailed = errors.New("skill registry: every enabled source failed")
)

// RegistrySummary is one search or detail result: what the source claims, plus the source's
// own safety verdict. The verdict is carried through to the human instead of being applied
// silently, because "isSuspicious" is somebody else's judgement about somebody else's text.
type RegistrySummary struct {
	Slug        string  `json:"slug"`
	DisplayName string  `json:"display_name"`
	Summary     string  `json:"summary"`
	Version     string  `json:"version"`
	Score       float64 `json:"score,omitempty"`

	MalwareBlocked bool   `json:"malware_blocked"`
	Suspicious     bool   `json:"suspicious"`
	URL            string `json:"url,omitempty"`
	Source         string `json:"source"`
}

// RegistryFile is one file of a bundle. Path is always slash-separated and relative to the
// skill root; Mode carries the executable bit, which is the only permission a skill keeps.
type RegistryFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// RegistryOrigin is what to remember about where a bundle came from. It is written next to the
// skill so a later reader can tell a hub install from a hand upload without guessing.
type RegistryOrigin struct {
	Source      string    `json:"source"`
	Slug        string    `json:"slug"`
	Version     string    `json:"version"`
	URL         string    `json:"url,omitempty"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

// archiveEntry pairs a validated archive path with its zip header, so contents are read only
// after every entry name has been judged.
type archiveEntry struct {
	name string
	file *zip.File
}

func archiveNames(entries []archiveEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}

// RegistryBundle is a downloaded, not-yet-written skill.
type RegistryBundle struct {
	Slug        string
	Name        string
	Description string
	Version     string
	Files       []RegistryFile
	Frontmatter map[string]string
	Origin      RegistryOrigin

	// suspicious is the source's own warning, carried separately because the human deciding
	// whether to install must see it, not have it silently swallowed by an allowlist.
	suspicious bool
}

// Registry is one community source. Fetch is the only required capability: a source such as a
// Git host has no meaningful keyword search, and pretending otherwise produces an endpoint that
// always errors.
type Registry interface {
	// Name is the stable identifier used in config and in provenance tags.
	Name() string
	// Fetch downloads one skill. slug is the source's own identifier ("owner/repo/dir" for
	// the Git source), and version pins a ref when given (empty means default branch).
	Fetch(ctx context.Context, slug, version string) (*RegistryBundle, error)
}

// Searcher is the optional capability of a registry that can answer a keyword query.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]RegistrySummary, error)
}

// RegistryTags are the provenance markers written alongside a hub install, so a later reader
// can tell it apart from a hand upload without trusting a name.
func (b *RegistryBundle) RegistryTags() []string {
	tags := []string{
		"source:" + b.Origin.Source,
		"hub:" + b.Origin.Slug,
		"hub-version:" + b.Version,
	}
	if b.suspicious {
		tags = append(tags, "hub-suspicious")
	}
	return tags
}

// IsSuspicious reports the source's own warning, which the approval prompt must show rather
// than swallow.
func (b *RegistryBundle) IsSuspicious() bool { return b.suspicious }

func (b *RegistryBundle) markSuspicious(v bool) { b.suspicious = v }

// applyFrontmatterIdentity takes the skill's own name, description and slug from SKILL.md, so
// the record written cannot disagree with the text the loader will read: a registry index row
// is the publisher's claim about a skill, the file is the skill.
//
// A frontmatter slug wins; otherwise the name is slugified the way an upload slugifies it.
func (b *RegistryBundle) applyFrontmatterIdentity() error {
	for _, f := range b.Files {
		if f.Path != "SKILL.md" {
			continue
		}
		name, description, slug, fields := ParseSkillFrontmatter(string(f.Content))
		if name == "" {
			return fmt.Errorf("%s: SKILL.md frontmatter has no name", b.identityRef())
		}
		b.Name, b.Description, b.Frontmatter = name, description, fields
		switch {
		case slug != "":
			b.Slug = slug
		case b.Slug == "":
			b.Slug = Slugify(name)
		}
		return nil
	}
	return fmt.Errorf("%s: bundle has no root SKILL.md", b.identityRef())
}

// identityRef names the bundle the way a human would look for it: the skill slug once known,
// otherwise where it came from.
func (b *RegistryBundle) identityRef() string {
	if b.Slug != "" {
		return b.Slug
	}
	if b.Origin.Slug != "" {
		return b.Origin.Source + ":" + b.Origin.Slug
	}
	return "skill"
}

// SkillMDPreview returns the head of SKILL.md for a human who is deciding whether to install
// it. The cut is made on a rune boundary so a multibyte description cannot be split, and limit
// <= 0 means "no preview".
func (b *RegistryBundle) SkillMDPreview(limit int) string {
	if limit <= 0 {
		return ""
	}
	for _, f := range b.Files {
		if f.Path != "SKILL.md" {
			continue
		}
		s := string(f.Content)
		if len(s) <= limit {
			return s
		}
		cut := limit
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		return s[:cut] + "\n…"
	}
	return ""
}

// Validate checks a bundle against the same rules a ZIP upload is held to: a single
// root SKILL.md, no traversal, no absolute paths, no symlink entries, no system
// artifacts, and bounded size and file count.
//
// It runs before anything touches disk on purpose. A bundle that fails leaves no trace.
func (b *RegistryBundle) Validate() error {
	if b == nil {
		return fmt.Errorf("%w: nil bundle", ErrUnsafeBundle)
	}
	if b.Slug == "" || !SlugRegexp.MatchString(b.Slug) {
		return fmt.Errorf("%w: unsafe slug %q", ErrUnsafeBundle, b.Slug)
	}
	if len(b.Files) == 0 {
		return fmt.Errorf("%w: empty bundle", ErrUnsafeBundle)
	}
	if len(b.Files) > registryMaxFiles {
		return fmt.Errorf("%w: %d files exceeds the %d cap", ErrUnsafeBundle, len(b.Files), registryMaxFiles)
	}

	total := int64(0)
	rootSkillMD := 0
	for _, f := range b.Files {
		if err := validateBundlePath(f.Path); err != nil {
			return err
		}
		// Only SKILL.md must have bytes. A community skill commonly ships an empty
		// scripts/<pkg>/__init__.py, and refusing the whole bundle for that would be a false
		// positive on the one file type the size ceilings cannot catch anyway.
		if f.Path == "SKILL.md" && len(f.Content) == 0 {
			return fmt.Errorf("%w: SKILL.md is empty", ErrUnsafeBundle)
		}
		if int64(len(f.Content)) > registryMaxFileBytes {
			return fmt.Errorf("%w: %s is %d bytes, over the %d per-file cap",
				ErrUnsafeBundle, f.Path, len(f.Content), registryMaxFileBytes)
		}
		total += int64(len(f.Content))
		if f.Path == "SKILL.md" {
			rootSkillMD++
		}
	}
	if rootSkillMD != 1 {
		return fmt.Errorf("%w: a skill needs exactly one root SKILL.md, found %d", ErrUnsafeBundle, rootSkillMD)
	}
	if total > registryMaxBundleBytes {
		return fmt.Errorf("%w: %d bytes total exceeds the %d cap",
			ErrUnsafeBundle, total, registryMaxBundleBytes)
	}
	return nil
}

func validateBundlePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty file path", ErrUnsafeBundle)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("%w: %s uses a backslash separator", ErrUnsafeBundle, p)
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: %s is absolute", ErrUnsafeBundle, p)
	}
	if cleaned := path.Clean(p); cleaned != p {
		return fmt.Errorf("%w: %s is not cleaned (a traversal shape)", ErrUnsafeBundle, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: %s escapes the skill directory", ErrUnsafeBundle, p)
		}
	}
	if IsSystemArtifact(p) {
		return fmt.Errorf("%w: %s is an archive artifact, not a skill file", ErrUnsafeBundle, p)
	}
	return nil
}
