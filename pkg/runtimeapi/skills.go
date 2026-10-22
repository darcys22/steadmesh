package runtimeapi

import "github.com/darcys22/steadmesh/pkg/spec"

const (
	PathSkillsBundle = "/v1/skills/bundle"
	// MaxSkillsBundleBytes bounds the complete JSON response, including base64 files.
	MaxSkillsBundleBytes = 16 << 20
	MaxSkillBundles      = 256
	MaxSkillsBundleFiles = 4096
)

// SkillBundle contains only server-authorized skill files, never plugin hooks,
// MCP servers, or executable plugin configuration. Files are relative to the skill.
type SkillBundle struct {
	Ref    spec.SkillRef         `json:"ref"`
	Source spec.SkillMarketplace `json:"source"`
	Digest string                `json:"digest"`
	Files  map[string][]byte     `json:"files"`
	Owner  string                `json:"owner"`
}

// SkillsBundleResponse is the full desired managed-skill set on each turn.
// An empty set means remove previously synchronized marketplace skills.
type SkillsBundleResponse struct {
	Skills []SkillBundle `json:"skills"`
}
