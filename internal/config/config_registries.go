package config

// SkillRegistriesConfig configures community skill sources. Nothing is contacted unless a
// source is explicitly enabled: an outbound fetch from a server that an operator never asked
// for is a surprise regardless of how harmless the destination looks.
type SkillRegistriesConfig struct {
	ClawHub *SkillRegistryConfig `json:"clawhub,omitempty"`
	GitHub  *SkillRegistryConfig `json:"github,omitempty"`
}

// SkillRegistryConfig is one source. BaseURL is the service root, and Token is a secret:
// it round-trips through config_secrets and never lands in config.json.
//
// For the Git source BaseURL is the API root (an internal GitHub Enterprise install), not a
// repository page; the repository itself is named per install as "owner/repo/path".
type SkillRegistryConfig struct {
	Enabled   *bool  `json:"enabled,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Token     string `json:"token,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// Enabled reports whether a source may be used. The pointer keeps "unset" different from
// "explicitly disabled", and both mean off.
func (c *SkillRegistryConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// AnyEnabled is used to decide whether the whole feature is worth exposing in the UI.
func (r *SkillRegistriesConfig) AnyEnabled() bool {
	return r != nil && (r.ClawHub.IsEnabled() || r.GitHub.IsEnabled())
}

// ensureRegistries returns the registries block, creating it on the way. A stored token is
// authoritative even when config.json never mentioned registries, so the secret overlay cannot
// write into a nil pointer.
func (c *Config) ensureRegistries() *SkillRegistriesConfig {
	if c.Skills.Registries == nil {
		c.Skills.Registries = &SkillRegistriesConfig{}
	}
	return c.Skills.Registries
}

func (r *SkillRegistriesConfig) ensureClawHub() *SkillRegistryConfig {
	if r.ClawHub == nil {
		r.ClawHub = &SkillRegistryConfig{}
	}
	return r.ClawHub
}

func (r *SkillRegistriesConfig) ensureGitHub() *SkillRegistryConfig {
	if r.GitHub == nil {
		r.GitHub = &SkillRegistryConfig{}
	}
	return r.GitHub
}
