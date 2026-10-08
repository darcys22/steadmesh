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
	// Timezone is the IANA time zone in which seats read and schedule times
	// of day, e.g. Australia/Melbourne. A channel binding can set its
	// human's own. Defaults to UTC.
	// +optional
	Timezone string `json:"timezone,omitempty"`

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
	// AccessProfiles grant seats practical access from their sandbox: tools,
	// network egress, credentials and a browser. Seats and teams name them;
	// a seat gets the union of its own and its teams' profiles.
	// +optional
	AccessProfiles map[string]AccessProfile `json:"access_profiles,omitempty"`
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
	// AccessProfiles are granted to every member.
	// +optional
	AccessProfiles []string `json:"access_profiles,omitempty"`
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
	// Adapter is the harness adapter name: claude-code, codex, pi or fake.
	Adapter string `json:"adapter"`
	// ImageDigest pins the harness image (image@sha256:... or a local tag in dev).
	ImageDigest string `json:"image_digest"`
	// Model selects the model and the connection that serves it.
	// +optional
	Model *ModelSelection `json:"model,omitempty"`
	// +optional
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	// Config holds adapter-specific options (docs/harnesses.html).
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// ModelSelection is a harness profile's model.
type ModelSelection struct {
	// Connection names a model connection.
	Connection string `json:"connection"`
	// ID is the model identifier sent to the endpoint. The platform rejects
	// requests from the seat for any other model.
	ID string `json:"id"`
	// API is the protocol: anthropic_messages, openai_responses or
	// openai_chat. When unset, the first API the harness speaks that the
	// connection and model also serve is used.
	// +optional
	API string `json:"api,omitempty"`
	// Settings are harness-specific model settings, e.g. reasoning_effort.
	// +optional
	Settings map[string]string `json:"settings,omitempty"`
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

// AccessProfile configures sandbox access plugins. Each field is one plugin;
// unset plugins grant nothing. The default is deny-all except the platform.
type AccessProfile struct {
	// Tools requires binaries in the seat image.
	// +optional
	Tools *ToolsAccess `json:"tools,omitempty"`
	// Egress allows HTTPS and HTTP to hosts through the egress gateway.
	// +optional
	Egress *EgressAccess `json:"egress,omitempty"`
	// Network allows direct connections to IP ranges (NetworkPolicy).
	// +optional
	Network *NetworkAccess `json:"network,omitempty"`
	// GitHub grants repository access through a github connection.
	// +optional
	GitHub *GitHubAccess `json:"github,omitempty"`
	// Browser gives the seat a headless browser as a tool.
	// +optional
	Browser *BrowserAccess `json:"browser,omitempty"`
}

type ToolsAccess struct {
	// Binaries that must be on the seat image's PATH, e.g. git, gh, curl.
	Binaries []string `json:"binaries"`
}

type EgressAccess struct {
	// Hosts the seat may reach: example.com, *.example.com (subdomains), or
	// host:port. Without a port, 443 and 80 are allowed.
	Hosts []string `json:"hosts"`
}

type NetworkAccess struct {
	Rules []NetworkRule `json:"rules"`
}

// NetworkRule allows direct egress to an IP range.
type NetworkRule struct {
	CIDR string `json:"cidr"`
	// Ports; empty allows every port.
	// +optional
	Ports []int64 `json:"ports,omitempty"`
	// Protocol is tcp (default), udp or sctp.
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

type GitHubAccess struct {
	// Connection is a github connection.
	Connection string `json:"connection"`
	// Repos are owner/name repositories the seat may use.
	Repos []string `json:"repos"`
	// Permissions maps contents, pull_requests, issues or metadata to read or write.
	Permissions map[string]string `json:"permissions"`
	// Delivery is platform (default: operations through the platform; the
	// credential never enters the sandbox) or sandbox (git and gh in the
	// sandbox receive a credential; see docs/sandbox.html).
	// +optional
	Delivery string `json:"delivery,omitempty"`
}

type BrowserAccess struct {
	// Session loads a signed-in browser session from a browser_session
	// connection. Without it browsing is anonymous.
	// +optional
	Session *BrowserSession `json:"session,omitempty"`
}

type BrowserSession struct {
	Connection string `json:"connection"`
}

type Connection struct {
	// Adapter is slack, linear, anthropic, openai, model or fake-* in tests.
	Adapter string `json:"adapter"`
	// +optional
	AccountID string `json:"account_id,omitempty"`
	// EndpointRef overrides the service base URL. For model connections it
	// is the API base, usually ending in /v1 (https://api.openai.com/v1).
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
	// Model describes a model endpoint: the APIs it serves and its models.
	// The anthropic and openai adapters have defaults; the model adapter
	// (any compatible endpoint) must declare its APIs.
	// +optional
	Model *ModelEndpoint `json:"model,omitempty"`
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// ModelEndpoint describes what a model connection serves. Claims are
// checked at readiness (Verify).
type ModelEndpoint struct {
	// APIs the endpoint serves: anthropic_messages, openai_responses, openai_chat.
	// +optional
	APIs []string `json:"apis,omitempty"`
	// Auth is how the credential is sent: bearer, x-api-key or header:<Name>.
	// +optional
	Auth string `json:"auth,omitempty"`
	// Models the endpoint serves. When set, harness profiles may only select
	// these, and readiness checks each one.
	// +optional
	Models []ModelEntry `json:"models,omitempty"`
	// Verify is how readiness checks the endpoint: request (a minimal request
	// per declared API and model), models (list models) or none.
	// +optional
	Verify string `json:"verify,omitempty"`
}

// ModelEntry is one model a connection serves.
type ModelEntry struct {
	ID string `json:"id"`
	// APIs restricts the APIs this model is served on; empty means all of
	// the connection's APIs.
	// +optional
	APIs []string `json:"apis,omitempty"`
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
	// AccessProfiles are granted to this seat, in addition to its teams'.
	// +optional
	AccessProfiles []string `json:"access_profiles,omitempty"`
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
	// Timezone is the human's IANA time zone; their representative reads
	// and schedules times of day in it. Defaults to the organisation's.
	// +optional
	Timezone string `json:"timezone,omitempty"`
}
