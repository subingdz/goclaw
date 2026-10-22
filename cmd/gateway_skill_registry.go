package cmd

import (
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/edition"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// setupSkillRegistries builds the community skill sources and registers the agent-facing install
// tool. It runs after ApplySystemConfigs for the same reason the rate limiter, allowed paths and
// STT providers do: a subsystem constructed from the JSON5 default before the DB overlay never
// sees a value the operator set in the panel.
//
// The returned set is also handed to the HTTP skills handler, so one construction serves both
// transports and one log line states the truth about what is enabled.
func setupSkillRegistries(
	cfg *config.Config,
	toolsReg *tools.Registry,
	pgStores *store.Stores,
	isOwner func(string) bool,
	dataDir string,
) *skills.Registries {
	registries := skills.BuildRegistries(cfg)
	if !registries.Enabled() {
		return registries
	}

	// Registering the tool needs the managed-skill store; a read-only or file-backed store
	// cannot accept an install, so the agent gets no tool rather than a tool that always fails.
	if toolsReg == nil || pgStores == nil || pgStores.Skills == nil {
		return registries
	}
	manageStore, ok := pgStores.Skills.(store.SkillManageStore)
	if !ok {
		slog.Warn("skills.registry: skill_install not registered (store is not manageable)")
		return registries
	}
	// Lite keeps skill self-management off entirely; a community install is the same class of
	// capability with a wider blast radius, so it follows the existing gate.
	if !edition.Current().TeamFullMode {
		slog.Info("skills.registry: skill_install unavailable in this edition", "edition", edition.Current().Name)
		return registries
	}

	toolsReg.Register(tools.NewSkillInstallTool(registries, manageStore, dataDir, isOwner))
	slog.Info("skill_install tool registered", "sources", registries.Names())
	return registries
}
