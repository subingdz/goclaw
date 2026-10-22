package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// skill_install lets an agent fetch a skill from a community source — but only on top of a human
// request, and only after the analysis has been put in front of that human in the same
// conversation. The enforcement is the two-call sequence below, not a trust assumption about the
// model's summary: install refuses unless an inspect for the same source+slug was recorded earlier
// in this session, and the installer itself refuses content the security guard flags.
//
// What this cannot prove is stated plainly: GoClaw has no way for a tool call to observe the
// human's reply, so "the human approved" is relayed by the agent. The blast radius is bounded by
// the facts that an install writes no grants, runs no dependency installs, and lands in the
// caller's own tenant skills-store.

const (
	// skillInstallProposalTTL is how long an inspect stays installable. Long enough to read an
	// analysis and answer; short enough that an analysis from yesterday cannot be approved today.
	skillInstallProposalTTL = 10 * time.Minute

	// skillInstallProposalCap prunes the proposal map so a busy gateway cannot grow it without
	// bound. Entries are small; the cap is a ceiling, not a feature.
	skillInstallProposalCap = 256

	// skillInstallPreviewChars caps how much SKILL.md the tool echoes. The agent has to relay
	// what the skill instructs, but a megabyte of prose is not an answer to a chat message.
	skillInstallPreviewChars = 1500
)

type SkillInstallTool struct {
	registries *skills.Registries
	installer  *skills.RegistryInstaller
	dataDir    string
	isOwner    func(string) bool

	mu        sync.Mutex
	proposals map[string]skillProposal
	now       func() time.Time
}

type skillProposal struct {
	createdAt time.Time
	status    string
}

func NewSkillInstallTool(registries *skills.Registries, manageStore store.SkillManageStore, dataDir string, isOwner func(string) bool) *SkillInstallTool {
	return &SkillInstallTool{
		registries: registries,
		installer:  skills.NewRegistryInstaller(manageStore),
		dataDir:    dataDir,
		isOwner:    isOwner,
		proposals:  map[string]skillProposal{},
		now:        time.Now,
	}
}

func (t *SkillInstallTool) Name() string { return "skill_install" }

func (t *SkillInstallTool) Description() string {
	return "Find and install skills from the community sources enabled on this gateway (ClawHub, a Git host). " +
		"action=search: keyword query across the sources. " +
		"action=inspect: download one skill and report its files, declared and missing dependencies, shell needs and security findings — you MUST relay that to the human and ask whether to install. " +
		"action=install: writes the skill into your tenant's skill store; it is refused unless you inspected the same source+slug in this conversation, and any security finding is a hard refusal. " +
		"Installing never grants the skill to any agent, and never installs dependencies. " +
		"Only use this when the human asked for a community skill — never to satisfy your own curiosity."
}

func (t *SkillInstallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"search", "inspect", "install"},
				"description": "Operation to perform.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Keyword query for action=search. There is no browse-all.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Source name (see action=search results, e.g. \"clawhub\", \"github\").",
			},
			"slug": map[string]any{
				"type":        "string",
				"description": "The source's own identifier: \"pdf\" on ClawHub, \"owner/repo/skills/pdf\" on a Git host. Required for inspect/install.",
			},
			"version": map[string]any{
				"type":        "string",
				"description": "Optional pin: a registry version, or a git ref. Empty means the source's latest.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max search results (default 10, ceiling 25).",
			},
		},
		"required": []string{"action"},
	}
}

func (t *SkillInstallTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t == nil || t.registries == nil || !t.registries.Enabled() {
		return ErrorResult("no community skill source is enabled on this gateway")
	}
	if res := t.checkTurn(ctx); res != nil {
		return res
	}

	action, _ := args["action"].(string)
	switch action {
	case "search":
		return t.executeSearch(ctx, args)
	case "inspect":
		return t.executeInspect(ctx, args)
	case "install":
		return t.executeInstall(ctx, args)
	default:
		return ErrorResult("action must be one of: search, inspect, install")
	}
}

// checkTurn is the provenance gate: this tool only exists for a human asking in a conversation.
// A cron, heartbeat, subagent or team run has no human to approve anything, and an owner check
// on a system role would not mean what it appears to mean.
func (t *SkillInstallTool) checkTurn(ctx context.Context) *Result {
	sessionKey := ToolSessionKeyFromCtx(ctx)
	switch {
	case sessionKey == "":
		return ErrorResult("skill_install needs a conversation session; there is none in this run")
	case sessions.IsCronSession(sessionKey):
		return ErrorResult("skill_install cannot run from a cron job: no human is there to approve the install")
	case sessions.IsHeartbeatSession(sessionKey):
		return ErrorResult("skill_install cannot run from a heartbeat: no human is there to approve the install")
	case sessions.IsSubagentSession(sessionKey):
		return ErrorResult("skill_install cannot run in a subagent: ask from the main conversation instead")
	case sessions.IsTeamSession(sessionKey):
		return ErrorResult("skill_install cannot run from a team task: the approving human must be in a direct conversation")
	}

	if !store.IsMasterScope(ctx) {
		return ErrorResult("skill_install is limited to the master scope on this gateway")
	}
	actor := store.ActorIDFromContext(ctx)
	if actor == "" {
		return ErrorResult("skill_install needs an identified user to own the installed skill")
	}
	if t.isOwner != nil && !t.isOwner(actor) {
		return ErrorResult("only the gateway owner may install community skills; ask the owner to do it from the Skills page")
	}
	return nil
}

func (t *SkillInstallTool) executeSearch(ctx context.Context, args map[string]any) *Result {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return ErrorResult("action=search needs a query")
	}
	limit := intArg(args, "limit", 10)
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	source, _ := args["source"].(string)

	cctx, cancel := context.WithTimeout(ctx, skills.RegistryRequestBudget)
	defer cancel()
	out, err := t.registries.SearchAll(cctx, strings.TrimSpace(query), strings.TrimSpace(source), limit)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if len(out.Summaries) == 0 {
		text := fmt.Sprintf("No skill matches %q.", query)
		if len(out.Failures) > 0 {
			text += " Failed sources: " + joinFailures(out.Failures)
		}
		return NewResult(text)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Community skills matching %q:\n\n", query)
	for _, s := range out.Summaries {
		flags := make([]string, 0, 2)
		if s.MalwareBlocked {
			flags = append(flags, "flagged malware by the source — cannot install")
		}
		if s.Suspicious {
			flags = append(flags, "flagged suspicious by the source")
		}
		flagText := ""
		if len(flags) > 0 {
			flagText = " [" + strings.Join(flags, ", ") + "]"
		}
		fmt.Fprintf(&sb, "- %s (%s: %s)%s\n  %s\n", s.DisplayName, s.Source, s.Slug, flagText, s.Summary)
	}
	if len(out.Failures) > 0 {
		sb.WriteString("\nSources that failed: " + joinFailures(out.Failures) + "\n")
	}
	sb.WriteString("\nNext: action=inspect with the matching source and slug, then show the analysis to the human.")
	return NewResult(sb.String())
}

func (t *SkillInstallTool) executeInspect(ctx context.Context, args map[string]any) *Result {
	target, err := t.targetFrom(args)
	if err != nil {
		return ErrorResult(err.Error())
	}

	cctx, cancel := context.WithTimeout(ctx, skills.RegistryRequestBudget)
	defer cancel()
	bundle, err := t.fetch(cctx, target)
	if err != nil {
		return ErrorResult(err.Error())
	}
	analysis, err := skills.AnalyzeBundle(bundle)
	if err != nil {
		return ErrorResult(err.Error())
	}

	t.recordProposal(ctx, target, analysis)

	text := renderAnalysis(target, &analysis, bundle.SkillMDPreview(skillInstallPreviewChars))
	if analysis.Unsafe {
		return NewResult(text + "\nThis bundle is refused by the content guard. Do not attempt to install it; tell the human why.")
	}
	return NewResult(text + "\nRelay this to the human and ask whether to install. If they agree, call action=install with the same source and slug in this conversation.")
}

func (t *SkillInstallTool) executeInstall(ctx context.Context, args map[string]any) *Result {
	target, err := t.targetFrom(args)
	if err != nil {
		return ErrorResult(err.Error())
	}

	key := proposalKey(ctx, target)
	if !t.consumeProposal(key) {
		return ErrorResult("install needs a matching action=inspect in this conversation first: the human has to see what the skill does before agreeing to it")
	}

	cctx, cancel := context.WithTimeout(ctx, skills.RegistryRequestBudget)
	defer cancel()
	bundle, err := t.fetch(cctx, target)
	if err != nil {
		return ErrorResult(err.Error())
	}

	res, err := t.installer.InstallBundle(cctx, t.tenantSkillsDir(ctx), bundle, store.ActorIDFromContext(ctx))
	if err != nil {
		// The proposal is consumed on purpose: an install that failed on content must not be
		// retryable against a stale analysis.
		return ErrorResult(err.Error())
	}

	slog.Info("skill_install: done",
		"source", target.source, "locator", target.slug, "skill", res.Slug,
		"version", res.Version, "status", res.Status, "session", ToolSessionKeyFromCtx(ctx))

	if res.Unchanged {
		return NewResult(fmt.Sprintf("%s v%d is already installed with identical content (status %s). Nothing changed, and no agent has access until a grant is created.",
			res.Slug, res.Version, res.Status))
	}
	text := fmt.Sprintf("Installed %s v%d (status %s) into %s. It is not granted to any agent, and no dependency was installed.",
		res.Slug, res.Version, res.Status, res.Dir)
	if len(res.Missing) > 0 {
		text += " It stays archived until these are available on this host: " + strings.Join(res.Missing, ", ") + "."
	}
	return NewResult(text)
}

// target is one resolved bundle request.
type installTarget struct {
	source  string
	slug    string
	version string
}

func (t *SkillInstallTool) targetFrom(args map[string]any) (installTarget, error) {
	target := installTarget{
		source:  strings.TrimSpace(stringArg(args, "source")),
		slug:    strings.TrimSpace(stringArg(args, "slug")),
		version: strings.TrimSpace(stringArg(args, "version")),
	}
	switch {
	case target.source == "":
		return target, errors.New("this action needs a source; run action=search to see the enabled ones")
	case target.slug == "":
		return target, errors.New("this action needs a slug")
	}
	if _, ok := t.registries.Get(target.source); !ok {
		return target, fmt.Errorf("unknown source %q; enabled sources: %s", target.source, strings.Join(t.registries.Names(), ", "))
	}
	return target, nil
}

func (t *SkillInstallTool) fetch(ctx context.Context, target installTarget) (*skills.RegistryBundle, error) {
	reg, ok := t.registries.Get(target.source)
	if !ok {
		return nil, fmt.Errorf("unknown source %q", target.source)
	}
	return reg.Fetch(ctx, target.slug, target.version)
}

func (t *SkillInstallTool) tenantSkillsDir(ctx context.Context) string {
	return config.TenantSkillsStoreDir(t.dataDir, store.TenantIDFromContext(ctx), store.TenantSlugFromContext(ctx))
}

// recordProposal marks this conversation as having seen the analysis for one bundle. An unsafe
// analysis is deliberately not recorded — there is nothing to approve.
func (t *SkillInstallTool) recordProposal(ctx context.Context, target installTarget, a skills.Analysis) {
	if a.Unsafe {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	t.proposals[proposalKey(ctx, target)] = skillProposal{createdAt: t.now().UTC(), status: a.Name}
}

// consumeProposal deletes the entry, so one inspect authorizes exactly one install.
func (t *SkillInstallTool) consumeProposal(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.proposals[key]
	if !ok {
		return false
	}
	delete(t.proposals, key)
	return t.now().UTC().Sub(p.createdAt) <= skillInstallProposalTTL
}

func (t *SkillInstallTool) pruneLocked() {
	now := t.now().UTC()
	for k, p := range t.proposals {
		if now.Sub(p.createdAt) > skillInstallProposalTTL {
			delete(t.proposals, k)
		}
	}
	if len(t.proposals) < skillInstallProposalCap {
		return
	}
	// Still over the cap: the oldest entries go, which only ever costs a human a second look.
	ages := make([]time.Time, 0, len(t.proposals))
	for _, p := range t.proposals {
		ages = append(ages, p.createdAt)
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].Before(ages[j]) })
	cutoff := ages[len(ages)-skillInstallProposalCap]
	for k, p := range t.proposals {
		if !p.createdAt.After(cutoff) {
			delete(t.proposals, k)
		}
	}
}

func proposalKey(ctx context.Context, target installTarget) string {
	return strings.Join([]string{ToolSessionKeyFromCtx(ctx), target.source, target.slug, target.version}, "|")
}

func renderAnalysis(target installTarget, a *skills.Analysis, preview string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill: %s\nFrom: %s:%s", a.Name, target.source, target.slug)
	if a.Version != "" {
		fmt.Fprintf(&sb, " @ %s", a.Version)
	}
	sb.WriteString("\n")
	if a.Description != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", a.Description)
	}
	fmt.Fprintf(&sb, "Files: %d, %s\n", len(a.Files), formatBytesShort(a.TotalBytes))
	for _, f := range a.Files {
		fmt.Fprintf(&sb, "  - %s (%s", f.Path, formatBytesShort(int64(f.Bytes)))
		if slices.Contains(a.Executables, f.Path) {
			sb.WriteString(", executable")
		}
		sb.WriteString(")\n")
	}
	if a.NeedsShell {
		sb.WriteString("Runs shell commands: yes (it ships scripts or asks for a shell)\n")
	}
	if len(a.Declared) > 0 {
		fmt.Fprintf(&sb, "Declared dependencies: %s\n", strings.Join(a.Declared, ", "))
	}
	if len(a.MissingDeps) > 0 {
		fmt.Fprintf(&sb, "Missing on this host: %s (the skill is registered archived until these exist)\n", strings.Join(a.MissingDeps, ", "))
	}
	if a.Suspicious {
		sb.WriteString("Risk flag: the source itself marks this skill as suspicious.\n")
	}
	if a.Unsafe {
		sb.WriteString("BLOCKED by the content guard:\n")
		for _, f := range a.Findings {
			fmt.Fprintf(&sb, "  - %s\n", f)
		}
	} else {
		sb.WriteString("Content guard: no findings.\n")
	}
	if preview != "" {
		fmt.Fprintf(&sb, "\nWhat SKILL.md tells the agent to do:\n%s\n", preview)
	}
	return sb.String()
}

func joinFailures(failures map[string]string) string {
	parts := make([]string, 0, len(failures))
	for src, reason := range failures {
		parts = append(parts, src+": "+reason)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// formatBytesShort keeps the file list readable in a chat message; a tool result is not a table.
func formatBytesShort(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
