package spec

const (
	SkillModeExplicit                      = "explicit"
	SkillModeDiscover                      = "discover"
	SkillModeAgentChoice                   = "agent_choice"
	DefaultMaxDynamicSkills          int64 = 16
	MaxDynamicSkills                 int64 = 128
	MaxStaticSkills                        = 128
	MaxSeatSkillMarketplaces               = 32
	MaxOrganizationSkillMarketplaces       = 128
)

// SkillMarketplace is an immutable public GitHub marketplace source. Only
// skill files are imported: plugin hooks, commands, agents and MCP servers are
// not installed, and skills never grant tools, credentials or network access.
type SkillMarketplace struct {
	// Repository is a public GitHub owner/repo, not a URL or local path.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`
	Repository string `json:"repository"`
	// Ref must be a full lowercase 40-hex commit SHA; branches and tags are forbidden.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{40}$`
	Ref string `json:"ref"`
}

// SkillPolicy bounds discovery and installation to declared sources. A nil
// policy disables marketplace skills. Static installs are supported in all
// modes; only agent_choice permits bounded dynamic installation.
type SkillPolicy struct {
	// Mode is explicit (default), discover (read-only catalog), or agent_choice.
	// +optional
	// +kubebuilder:validation:Enum=explicit;discover;agent_choice
	Mode string `json:"mode,omitempty"`
	// Marketplaces is the explicit allowlist of organization marketplace keys.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	Marketplaces []string `json:"marketplaces"`
	// Install selects individual skills, not entire plugins. Each marketplace
	// must appear in marketplaces. No plugin executable components are enabled.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Install []SkillRef `json:"install,omitempty"`
	// MaxDynamicSkills bounds agent-selected skills, excluding static installs.
	// Only valid in agent_choice mode; omitted/zero defaults to 16 in the compiler.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	MaxDynamicSkills int64 `json:"max_dynamic_skills,omitempty"`
}

// SkillRef identifies one skill inside one plugin of a pinned marketplace.
type SkillRef struct {
	Marketplace string `json:"marketplace"`
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9_-]{0,127}$`
	Plugin string `json:"plugin"`
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9_-]{0,127}$`
	Skill string `json:"skill"`
}
