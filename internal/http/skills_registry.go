package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Community skill registry endpoints.
//
// The panel asks a source what exists, asks GoClaw to look inside one result, and only then asks
// for it to be written. Each step is a separate request because the human approving the install
// must see the same analysis the installer enforces, not a summary of it.
//
// Nothing here is enabled by default: a source has to be switched on in config, and every route
// is master-scope. A gateway that fetches outbound on behalf of a tenant admin who never asked
// for it is not a feature.

// registryPreviewBytes bounds how much of SKILL.md the inspect endpoint echoes back. Enough for
// a human to read what the skill actually tells an agent to do, small enough that a megabyte of
// frontmatter prose cannot be used to inflate a panel response.
const registryPreviewBytes = 4096

// SetRegistries wires community sources into the skills handler. Called at startup only; a
// registry added mid-request would have no provenance record anyway.
func (h *SkillsHandler) SetRegistries(regs *skills.Registries) {
	h.registries = regs
	h.registryInstaller = nil
	if regs.Enabled() {
		h.registryInstaller = skills.NewRegistryInstaller(h.skills)
	}
}

// registryTarget is the request body for inspect and install. Source is the registry name, Slug
// is that source's own identifier ("pdf" on ClawHub, "owner/repo/skills/pdf" on a Git host), and
// Version pins a ref where the source supports it.
type registryTarget struct {
	Source  string `json:"source"`
	Slug    string `json:"slug"`
	Version string `json:"version,omitempty"`
}

func (t registryTarget) validate(locale string) (string, bool) {
	switch {
	case strings.TrimSpace(t.Source) == "":
		return i18n.T(locale, i18n.MsgRequired, "source"), false
	case strings.TrimSpace(t.Slug) == "":
		return i18n.T(locale, i18n.MsgRequired, "slug"), false
	}
	return "", true
}

// registryGate reports whether the feature is usable, writing the response when it is not.
// A disabled source answers 503 rather than 404 so the panel can say "enable it in config"
// instead of looking like a broken build.
func (h *SkillsHandler) registryGate(w http.ResponseWriter, r *http.Request) bool {
	if h.registries.Enabled() {
		return true
	}
	locale := store.LocaleFromContext(r.Context())
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": i18n.T(locale, i18n.MsgSkillRegistryDisabled),
	})
	return false
}

// requireRegistryOwner gates the one route that writes to disk. Master scope alone is not
// enough: installing a community skill changes what every agent on this gateway may eventually
// be granted, so it is an owner decision.
func requireRegistryOwner(w http.ResponseWriter, r *http.Request) bool {
	if !requireMasterScope(w, r) {
		return false
	}
	if store.IsOwnerRole(r.Context()) {
		return true
	}
	locale := store.LocaleFromContext(r.Context())
	slog.Warn("security.registry_role_denied",
		"path", r.URL.Path, "user_id", store.UserIDFromContext(r.Context()))
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": i18n.T(locale, i18n.MsgPermissionDenied, "skill registry install"),
	})
	return false
}

// GET /v1/skills/registry/sources — what is switched on, and which of those can answer a query.
func (h *SkillsHandler) handleRegistrySources(w http.ResponseWriter, r *http.Request) {
	sources := h.registries.Sources()
	if sources == nil {
		sources = []skills.RegistrySource{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": h.registries.Enabled(),
		"sources": sources,
	})
}

// GET /v1/skills/registry/search?q=&source=&limit= — fan-out across the enabled sources.
func (h *SkillsHandler) handleRegistrySearch(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	if !h.registryGate(w, r) {
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgRequired, "q")})
		return
	}
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source != "" {
		if _, ok := h.registries.Get(source); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgSkillRegistrySourceUnknown, source)})
			return
		}
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "limit must be a positive number")})
			return
		}
		if n > 50 {
			n = 50
		}
		limit = n
	}

	ctx, cancel := registryRequestContext(r)
	defer cancel()
	outcome, err := h.registries.SearchAll(ctx, query, source, limit)
	if err != nil {
		writeRegistryError(w, r, err, outcome.Failures)
		return
	}

	results := outcome.Summaries
	if results == nil {
		results = []skills.RegistrySummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":    query,
		"results":  results,
		"failures": outcome.Failures,
	})
}

// POST /v1/skills/registry/inspect — download one bundle and report what it contains.
// Writes nothing: the bundle is staged in a temp dir that is removed before responding.
func (h *SkillsHandler) handleRegistryInspect(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	if !h.registryGate(w, r) {
		return
	}

	var target registryTarget
	if !bindJSON(w, r, locale, &target) {
		return
	}
	if msg, ok := target.validate(locale); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	ctx, cancel := registryRequestContext(r)
	defer cancel()
	bundle, ok := h.registryBundleFor(w, r, ctx, target)
	if !ok {
		return
	}
	analysis, err := skills.AnalyzeBundle(bundle)
	if err != nil {
		writeRegistryError(w, r, err, nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"analysis": analysis,
		"skill_md": bundle.SkillMDPreview(registryPreviewBytes),
	})
}

// POST /v1/skills/registry/install — download, re-analyse, and write into the tenant store.
//
// The install path analyses again rather than trusting a payload from the client: an approval
// that could be replayed with a different bundle body would be a formality, not a control.
func (h *SkillsHandler) handleRegistryInstall(w http.ResponseWriter, r *http.Request) {
	locale := store.LocaleFromContext(r.Context())
	if !h.registryGate(w, r) {
		return
	}
	if !requireRegistryOwner(w, r) {
		return
	}
	var target registryTarget
	if !bindJSON(w, r, locale, &target) {
		return
	}
	if msg, ok := target.validate(locale); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidRequest, "a user identity is required to own an installed skill")})
		return
	}

	ctx, cancel := registryRequestContext(r)
	defer cancel()
	bundle, ok := h.registryBundleFor(w, r, ctx, target)
	if !ok {
		return
	}

	result, err := h.registryInstaller.InstallBundle(ctx, h.tenantSkillsDir(r), bundle, userID)
	if err != nil {
		slog.Warn("skills.registry: install failed",
			"source", target.Source, "slug", target.Slug, "user_id", userID, "error", err)
		writeRegistryError(w, r, err, nil)
		return
	}

	if !result.Unchanged {
		h.emitCacheInvalidate(bus.CacheKindSkills, result.ID.String(), uuid.Nil)
		emitAudit(h.msgBus, r, "skill.registry_installed", "skill", result.Slug)
		slog.Info("skills.registry: installed",
			"source", target.Source, "locator", target.Slug, "skill", result.Slug,
			"version", result.Version, "status", result.Status, "by", userID)
	}

	status := http.StatusCreated
	if result.Unchanged {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

// registryBundleFor resolves the named source and downloads one bundle, writing the error
// response itself so the two callers stay three lines long.
func (h *SkillsHandler) registryBundleFor(w http.ResponseWriter, r *http.Request, ctx context.Context, target registryTarget) (*skills.RegistryBundle, bool) {
	reg, ok := h.registries.Get(target.Source)
	if !ok {
		locale := store.LocaleFromContext(r.Context())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgSkillRegistrySourceUnknown, target.Source)})
		return nil, false
	}
	bundle, err := reg.Fetch(ctx, target.Slug, target.Version)
	if err != nil {
		slog.Warn("skills.registry: fetch failed", "source", target.Source, "slug", target.Slug, "error", err)
		writeRegistryError(w, r, err, nil)
		return nil, false
	}
	return bundle, true
}

// registryRequestContext bounds one registry round trip. The budget is the skills package's, so
// a handler cannot outlive the client it is calling and leave a bundle half-written.
func registryRequestContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), skills.RegistryRequestBudget)
}

// writeRegistryError maps the sentinels to statuses that mean different things to whoever is
// reading: 403 is a content decision, 409 is a naming collision, 422 is a malformed bundle,
// 502 is the source being unavailable, and 503 is this gateway having the feature off.
func writeRegistryError(w http.ResponseWriter, r *http.Request, err error, failures map[string]string) {
	locale := store.LocaleFromContext(r.Context())
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, skills.ErrNoRegistries):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": i18n.T(locale, i18n.MsgSkillRegistryDisabled)})
		return
	case errors.Is(err, skills.ErrRegistryNotSearchable):
		status = http.StatusBadRequest
	case errors.Is(err, skills.ErrAllRegistriesFailed):
		msg := err.Error()
		if len(failures) > 0 {
			parts := make([]string, 0, len(failures))
			for src, reason := range failures {
				parts = append(parts, src+": "+reason)
			}
			msg = strings.Join(parts, "; ")
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
		return
	case errors.Is(err, skills.ErrInstallConflict):
		status = http.StatusConflict
	case errors.Is(err, skills.ErrRegistryRefused), errors.Is(err, skills.ErrInstallRefused):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": i18n.T(locale, i18n.MsgSkillRegistryRefused, err.Error())})
		return
	case errors.Is(err, skills.ErrUnsafeBundle):
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
