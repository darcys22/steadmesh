// Package spec defines the organisation specification shared by the Terraform
// provider, the Kubernetes API types and the controller. It is the single
// source of truth for field names and validation (design §4.6).
//
// +kubebuilder:object:generate=true
package spec

// SchemaVersion is the version of the organisation specification schema.
const SchemaVersion = "v1alpha1"

// DefaultsVersion identifies the set of defaults applied by the compiler. It is
// recorded in every effective manifest so that defaults are visible (§4.6).
const DefaultsVersion = 1

// MaxSpecBytes is the documented size limit of a serialised specification
// (§13.3, ADR-0007). Large instruction documents must be referenced.
const MaxSpecBytes = 256 * 1024

// OrganizationSpec is the declared organisation. Maps are keyed by stable keys.
type OrganizationSpec struct {
	// Key is the stable organisation key.
	Key string `json:"key"`
	// DisplayName is the human-readable organisation name.
	DisplayName string `json:"display_name"`
	// DataRetention applies to the organisation when it is deleted: retain or delete.
	// +optional
	DataRetention string `json:"data_retention,omitempty"`

	// +optional
	CultureRefs []string `json:"culture_refs,omitempty"`
	// +optional
	TeamTemplates map[string]TeamTemplate `json:"team_templates,omitempty"`
	// +optional
	Teams map[string]Team `json:"teams,omitempty"`
	// +optional
	MemoryStores map[string]MemoryStore `json:"memory_stores,omitempty"`
	// +optional
	SharedWorkspaces map[string]SharedWorkspace `json:"shared_workspaces,omitempty"`
	// +optional
	HarnessProfiles map[string]HarnessProfile `json:"harness_profiles,omitempty"`
	// +optional
	ExecutionProfiles map[string]ExecutionProfile `json:"execution_profiles,omitempty"`
	// +optional
	SandboxProfiles map[string]SandboxProfile `json:"sandbox_profiles,omitempty"`
	// +optional
	Connections map[string]Connection `json:"connections,omitempty"`
	// +optional
	Seats map[string]Seat `json:"seats,omitempty"`
	// +optional
	Grants map[string]Grant `json:"grants,omitempty"`
	// +optional
	MessageRoutes map[string]MessageRoute `json:"message_routes,omitempty"`
	// +optional
	ChannelBindings map[string]ChannelBinding `json:"channel_bindings,omitempty"`
	// WorkPublication optionally publishes work items to a tracker so people
	// can follow progress there. Internal coordination never depends on it.
	// +optional
	WorkPublication *WorkPublication `json:"work_publication,omitempty"`
}

// WorkPublication publishes the work items of shared memory stores to a
// tracker connection. Personal stores are never published.
type WorkPublication struct {
	// Connection is a tracker connection, e.g. a Linear connection.
	Connection string `json:"connection"`
	// Stores are the shared memory stores whose work items are published.
	Stores []string `json:"stores"`
}

// TeamTemplate is a reusable team definition. Templates may extend one other
// template; merge rules are documented in pkg/compile.
type TeamTemplate struct {
	// +optional
	Extends string `json:"extends,omitempty"`
	// +optional
	InstructionRefs []string `json:"instruction_refs,omitempty"`
	// Roles maps a role key to its role instruction reference.
	// +optional
	Roles map[string]string `json:"roles,omitempty"`
	// SharedMemory names memory stores granted to the team instantiating the template.
	// +optional
	SharedMemory map[string][]string `json:"shared_memory,omitempty"`
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// Team is a concrete grouping of seats. Membership is declared on seats only.
type Team struct {
	// +optional
	Template string `json:"template,omitempty"`
	// +optional
	InstructionRefs []string `json:"instruction_refs,omitempty"`
	// SharedMemory maps memory store keys to operations granted to members.
	// +optional
	SharedMemory map[string][]string `json:"shared_memory,omitempty"`
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

type MemoryStore struct {
	// Retention is retain or delete.
	// +optional
	Retention string `json:"retention,omitempty"`
	// +optional
	BackingClass string `json:"backing_class,omitempty"`
	// +optional
	CapacityMB int64 `json:"capacity_mb,omitempty"`
}

type SharedWorkspace struct {
	// +optional
	StorageClass string `json:"storage_class,omitempty"`
	// AccessMode is ReadWriteMany or ReadOnlyMany.
	AccessMode string `json:"access_mode"`
	// +optional
	SizeGB int64 `json:"size_gb,omitempty"`
	// +optional
	Retention string `json:"retention,omitempty"`
}

type HarnessProfile struct {
	// Adapter is the harness adapter name, e.g. claude-code or fake.
	Adapter string `json:"adapter"`
	// ImageDigest pins the harness image (image@sha256:... or a local tag in dev).
	ImageDigest string `json:"image_digest"`
	// ModelConnection names a connection with a model adapter.
	// +optional
	ModelConnection string `json:"model_connection,omitempty"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

type ExecutionProfile struct {
	// Backend is the sandbox backend, initially kubernetes.
	Backend string `json:"backend"`
	// ServiceClass is interactive or background.
	// +optional
	ServiceClass string `json:"service_class,omitempty"`
	// IdlePolicy is warm, warm_then_stop or suspend.
	// +optional
	IdlePolicy string `json:"idle_policy,omitempty"`
	// +optional
	IdleTimeout string `json:"idle_timeout,omitempty"`
	// +optional
	CPURequest string `json:"cpu_request,omitempty"`
	// +optional
	CPULimit string `json:"cpu_limit,omitempty"`
	// +optional
	MemoryRequest string `json:"memory_request,omitempty"`
	// +optional
	MemoryLimit string `json:"memory_limit,omitempty"`
	// +optional
	WorkspaceSizeGB int64 `json:"workspace_size_gb,omitempty"`
	// StorageClass is the storage class of the seat workspace volume. Empty
	// uses the cluster default.
	// +optional
	StorageClass string `json:"storage_class,omitempty"`
	// +optional
	RequiredFeatures []string `json:"required_features,omitempty"`
}

type SandboxProfile struct {
	// NetworkPolicy is deny_all_except_platform (default) or a named policy ref.
	// +optional
	NetworkPolicyRef string `json:"network_policy_ref,omitempty"`
	// +optional
	FilesystemPolicyRef string `json:"filesystem_policy_ref,omitempty"`
	// +optional
	RuntimeClass string `json:"runtime_class,omitempty"`
	// +optional
	RequiredEnforcement []string `json:"required_enforcement,omitempty"`
}

type Connection struct {
	// Adapter is slack, linear, anthropic or fake-* in tests.
	Adapter string `json:"adapter"`
	// +optional
	AccountID string `json:"account_id,omitempty"`
	// +optional
	EndpointRef string `json:"endpoint_ref,omitempty"`
	// SecretRef points at a secret in a supported manager (vault:..., k8s:...).
	// +optional
	SecretRef string `json:"secret_ref,omitempty"`
	// Ownership is external (default, never deleted) or managed.
	// +optional
	Ownership string `json:"ownership,omitempty"`
	// Required connections must authenticate before the organisation is ready.
	// When unset it is decided by role (compile.RequiredConnections).
	// +optional
	Required *bool `json:"required,omitempty"`
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

type Seat struct {
	RoleRef string `json:"role_ref"`
	// +optional
	Teams            []string `json:"teams,omitempty"`
	HarnessProfile   string   `json:"harness_profile"`
	ExecutionProfile string   `json:"execution_profile"`
	SandboxProfile   string   `json:"sandbox_profile"`
	// +optional
	PersonalMemory string `json:"personal_memory,omitempty"`
	// +optional
	Workspace *WorkspaceBinding `json:"workspace,omitempty"`
	// +optional
	DisplayName string `json:"display_name,omitempty"`
	// +optional
	InstructionRefs []string `json:"instruction_refs,omitempty"`
	// AdoptFrom explicitly adopts the retained data of a retired seat ID (§4.1).
	// +optional
	AdoptFrom string `json:"adopt_from,omitempty"`
}

type WorkspaceBinding struct {
	// +optional
	Persistent bool `json:"persistent,omitempty"`
	// +optional
	Shared []string `json:"shared,omitempty"`
}

type Grant struct {
	// Subject is seat:<key> or team:<key>.
	Subject string `json:"subject"`
	// Resource is connection:<key>, memory:<key> or workspace:<key>.
	Resource   string   `json:"resource"`
	Operations []string `json:"operations"`
	// +optional
	Targets []string `json:"targets,omitempty"`
}

type MessageRoute struct {
	// From is seat:<key> or team:<key>.
	From string `json:"from"`
	// To is seat:<key> or team:<key>.
	To string `json:"to"`
	// Reply permits the recipient to reply within conversations opened on this route.
	// +optional
	Reply bool `json:"reply,omitempty"`
	// Bidirectional permits both directions.
	// +optional
	Bidirectional bool `json:"bidirectional,omitempty"`
}

type ChannelBinding struct {
	Connection     string `json:"connection"`
	ExternalUserID string `json:"external_user_id"`
	Seat           string `json:"seat"`
	// Mode is direct_message.
	// +optional
	Mode string `json:"mode,omitempty"`
}
