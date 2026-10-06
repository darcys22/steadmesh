package compile

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
	// NeedsModel means the harness requires a model connection.
	NeedsModel bool
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
		Harnesses: map[string]HarnessInfo{
			"claude-code": {
				Capabilities: []string{"tools", "mcp", "event_stream", "session_resume", "interrupt", "application_checkpoint"},
				NeedsModel:   true,
			},
			"fake": {
				Capabilities: []string{"tools", "event_stream", "interrupt", "application_checkpoint"},
			},
		},
		Backends: map[string]BackendInfo{
			"kubernetes": {Features: []string{
				"warm_idle", "application_stop_restore", "network_policy", "resource_limits",
				"persistent_workspace", "runtime_class", "non_root", "read_only_root", "seccomp",
			}},
		},
		Connectors: map[string]ConnectorInfo{
			"slack":     {Kind: "communication", Operations: []string{"channel.reply"}},
			"linear":    {Kind: "tracker", Operations: []string{"project.read", "project.create", "task.read", "task.write", "comment.write"}},
			"anthropic": {Kind: "model", Operations: []string{"model.infer"}},
		},
		IdlePolicies: map[string]string{
			"warm":           "warm_idle",
			"warm_then_stop": "application_stop_restore",
			"suspend":        "process_suspend",
		},
	}
}
