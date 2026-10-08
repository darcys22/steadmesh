package compile

import (
	"github.com/darcys22/steadmesh/harnesses"
	// Every built-in harness registers its descriptor.
	_ "github.com/darcys22/steadmesh/harnesses/all"
)

// Catalog describes the adapters and backends this platform release supports.
// Compatibility validation (§4.4) checks declarations against it; unsupported
// combinations fail explicitly rather than falling back to weaker behaviour.
type Catalog struct {
	Harnesses    map[string]HarnessInfo
	Backends     map[string]BackendInfo
	Connectors   map[string]ConnectorInfo
	IdlePolicies map[string]string // idle policy -> required backend feature
}

type HarnessInfo struct {
	Capabilities []string
	// NeedsModel means the harness requires a model.
	NeedsModel bool
	// APIs are the model APIs the harness speaks, in order of preference.
	APIs []string
	// Settings are the model.settings keys the harness accepts.
	Settings map[string]harnesses.Setting
}

type BackendInfo struct {
	Features []string
}

type ConnectorInfo struct {
	// Operations a grant may name on a connection of this adapter.
	Operations []string
	// Kind is communication, tracker or model.
	Kind string
	// ManagedOwnership means the adapter can create and own external resources.
	ManagedOwnership bool
	// ModelAPIs are the APIs a model connection serves when it declares
	// none. Empty for a model adapter means the connection must declare them.
	ModelAPIs []string
	// DefaultAuth is how a model connection sends its credential by default.
	DefaultAuth string
}

// Personal-memory operations implicitly granted to the seat that selects the store (§5.2).
var PersonalMemoryOperations = []string{"read", "search", "write", "revise", "archive", "publish", "history"}

// MemoryOperations are the operations a grant may name on a memory store.
var MemoryOperations = []string{"read", "search", "write", "revise", "archive", "publish", "history"}

// WorkspaceOperations are the operations a grant may name on a shared workspace.
var WorkspaceOperations = []string{"read", "write"}

// DefaultCatalog is the catalog of the first release.
func DefaultCatalog() Catalog {
	return Catalog{
		Harnesses: registeredHarnesses(),
		Backends: map[string]BackendInfo{
			"kubernetes": {Features: []string{
				"warm_idle", "application_stop_restore", "network_policy", "resource_limits",
				"persistent_workspace", "runtime_class", "non_root", "read_only_root", "seccomp",
			}},
		},
		Connectors: map[string]ConnectorInfo{
			"slack":     {Kind: "communication", Operations: []string{"channel.reply"}},
			"terminal":  {Kind: "communication", Operations: []string{"channel.reply"}},
			"linear":    {Kind: "tracker", Operations: []string{"project.read", "project.create", "task.read", "task.write", "comment.read", "comment.write"}},
			"anthropic": {Kind: "model", Operations: []string{"model.infer"}, ModelAPIs: []string{harnesses.APIAnthropicMessages}, DefaultAuth: "x-api-key"},
			"openai":    {Kind: "model", Operations: []string{"model.infer"}, ModelAPIs: []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat}, DefaultAuth: "bearer"},
			"github":    {Kind: "code_host", Operations: []string{"repo.read", "pull_request.read", "pull_request.create", "issue.read", "issue.comment"}},
			// A signed-in browser session (Playwright storage state) for the browser plugin.
			"browser_session": {Kind: "credential"},
			// Any compatible endpoint; it declares the APIs it serves.
			"model": {Kind: "model", Operations: []string{"model.infer"}, DefaultAuth: "bearer"},
		},
		IdlePolicies: map[string]string{
			"warm":           "warm_idle",
			"warm_then_stop": "application_stop_restore",
			"suspend":        "process_suspend",
		},
	}
}

// registeredHarnesses describes every registered harness adapter, under its
// name and aliases.
func registeredHarnesses() map[string]HarnessInfo {
	out := map[string]HarnessInfo{}
	for _, d := range harnesses.Registered() {
		info := HarnessInfo{Capabilities: d.Capabilities, NeedsModel: d.NeedsModel, APIs: d.APIs, Settings: d.Settings}
		out[d.Name] = info
		for _, a := range d.Aliases {
			out[a] = info
		}
	}
	return out
}
