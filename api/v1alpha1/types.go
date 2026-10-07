package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/pkg/spec"
)

// Well-known labels, annotations and finalizers.
const (
	LabelOrganization = "steadmesh.io/organization"
	LabelSeat         = "steadmesh.io/seat"
	LabelComponent    = "steadmesh.io/component"

	// AnnotationManagedBy records the declaration owner (terraform or helm, §3.1).
	AnnotationManagedBy = "steadmesh.io/managed-by"
	// AnnotationSeatID maps a service account to its durable seat identity.
	AnnotationSeatID = "steadmesh.io/seat-id"
	AnnotationOrgID  = "steadmesh.io/organization-id"

	FinalizerOrganization = "steadmesh.io/organization"
	FinalizerSeat         = "steadmesh.io/seat"

	// GatewayAudience is the only audience of tokens projected into seat Pods.
	GatewayAudience = "steadmesh-gateway"

	// AnnotationVerifyRequest carries a nonce requesting a fresh verification
	// pass; the controller echoes it in AnnotationVerifyObserved once the pass
	// for that nonce has completed (contracts.md, Readiness flow).
	AnnotationVerifyRequest  = "steadmesh.io/verify-request"
	AnnotationVerifyObserved = "steadmesh.io/verify-observed"
)

// Condition types (§5.4).
const (
	CondConfigured               = "Configured"
	CondStorageReady             = "StorageReady"
	CondIdentitiesReady          = "IdentitiesReady"
	CondHarnessCompatible        = "HarnessCompatible"
	CondSandboxEnforced          = "SandboxEnforced"
	CondConnectionsAuthenticated = "ConnectionsAuthenticated"
	CondIngressReady             = "IngressReady"
	CondBindingsValid            = "BindingsValid"
	CondRoutesExecutable         = "RoutesExecutable"
	// CondIntegrationsDegraded is informational: True while an optional
	// connection (one OperationalReady does not require) is failing.
	CondIntegrationsDegraded = "IntegrationsDegraded"
	CondOperationalReady     = "OperationalReady"
)

// ExecutionState is the technical execution state of a seat (§6.2).
type ExecutionState string

const (
	StateProvisioning ExecutionState = "Provisioning"
	StateWarm         ExecutionState = "Warm"
	StateExecuting    ExecutionState = "Executing"
	StateQuiescing    ExecutionState = "Quiescing"
	StateSuspended    ExecutionState = "Suspended"
	StateStopped      ExecutionState = "Stopped"
	StateRecovering   ExecutionState = "Recovering"
	StateBlocked      ExecutionState = "Blocked"
	StateRetired      ExecutionState = "Retired"
)

// AgentOrganizationSpec is the resolved organisation declaration.
type AgentOrganizationSpec struct {
	spec.OrganizationSpec `json:",inline"`
}

type SeatSummary struct {
	SeatID          string         `json:"seatID"`
	State           ExecutionState `json:"state,omitempty"`
	Ready           bool           `json:"ready"`
	ConfigRevision  string         `json:"configRevision,omitempty"`
	AdoptedRevision string         `json:"adoptedRevision,omitempty"`
	Reason          string         `json:"reason,omitempty"`
}

type RepresentativeEndpoint struct {
	Seat           string `json:"seat"`
	SeatID         string `json:"seatID"`
	Connection     string `json:"connection"`
	Adapter        string `json:"adapter"`
	ExternalUserID string `json:"externalUserID"`
	Mode           string `json:"mode"`
}

// ConnectionDetails are stable, non-secret details returned after readiness.
type ConnectionDetails struct {
	OrganizationID  string                            `json:"organizationID,omitempty"`
	Namespace       string                            `json:"namespace,omitempty"`
	StatusEndpoint  string                            `json:"statusEndpoint,omitempty"`
	Representatives map[string]RepresentativeEndpoint `json:"representatives,omitempty"`
}

type AgentOrganizationStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	OrganizationID     string             `json:"organizationID,omitempty"`
	EffectiveRevision  string             `json:"effectiveRevision,omitempty"`
	DefaultsVersion    int                `json:"defaultsVersion,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Seats map[string]SeatSummary `json:"seats,omitempty"`
	// +optional
	ConnectionDetails *ConnectionDetails `json:"connectionDetails,omitempty"`
	// LastVerifiedTime is the time of the last successful synthetic verification.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=aorg
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="OperationalReady")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="OperationalReady")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentOrganization declares an organisation of persistent agents.
type AgentOrganization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentOrganizationSpec   `json:"spec,omitempty"`
	Status AgentOrganizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AgentOrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentOrganization `json:"items"`
}

// ToolCapability is one entry of a seat's effective tool manifest. It contains
// no secret values.
type ToolCapability struct {
	Resource   string   `json:"resource"`
	Operations []string `json:"operations"`
	// +optional
	Targets []string `json:"targets,omitempty"`
}

type WorkspaceRef struct {
	// ClaimName is the seat's persistent workspace claim.
	ClaimName string `json:"claimName"`
	// +optional
	Shared []string `json:"shared,omitempty"`
}

// AgentSeatSpec is generated by the controller from the organisation.
type AgentSeatSpec struct {
	OrganizationName string `json:"organizationName"`
	OrganizationID   string `json:"organizationID"`
	SeatKey          string `json:"seatKey"`
	// SeatID is the immutable durable identity of the seat.
	SeatID           string           `json:"seatID"`
	ConfigRevision   string           `json:"configRevision"`
	HarnessProfile   string           `json:"harnessProfile"`
	ExecutionProfile string           `json:"executionProfile"`
	SandboxProfile   string           `json:"sandboxProfile"`
	Workspace        WorkspaceRef     `json:"workspace"`
	Tools            []ToolCapability `json:"tools,omitempty"`
	// ManifestConfigMap holds the resolved seat manifest and instruction texts.
	ManifestConfigMap string `json:"manifestConfigMap"`
	// Retired is set when the seat is removed from the organisation.
	// +optional
	Retired bool `json:"retired,omitempty"`
	// AdminSuspended is an infrastructure control that stops execution (§14).
	// +optional
	AdminSuspended bool `json:"adminSuspended,omitempty"`
}

type AgentSeatStatus struct {
	ObservedGeneration int64          `json:"observedGeneration,omitempty"`
	ExecutionState     ExecutionState `json:"executionState,omitempty"`
	// BackendRef identifies the backend instance (e.g. statefulset/<name>).
	BackendRef string `json:"backendRef,omitempty"`
	// LatestCheckpoint describes the latest recovery checkpoint and its guarantee.
	LatestCheckpoint string `json:"latestCheckpoint,omitempty"`
	LeaseGeneration  int64  `json:"leaseGeneration,omitempty"`
	AdoptedRevision  string `json:"adoptedRevision,omitempty"`
	// +optional
	LastActivityTime *metav1.Time       `json:"lastActivityTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	// LastError is redacted.
	LastError string `json:"lastError,omitempty"`
	// Probe is the synthetic readiness probe of the current config revision.
	// +optional
	Probe *SeatProbeStatus `json:"probe,omitempty"`
}

// SeatProbeStatus records one synthetic execution probe (§5.4).
type SeatProbeStatus struct {
	ID string `json:"id"`
	// Revision is the config revision the probe verified.
	Revision string `json:"revision"`
	// Nonce is the verify-request nonce the probe answered, if any.
	// +optional
	Nonce string `json:"nonce,omitempty"`
	// Status is pending, passed or failed.
	Status string `json:"status"`
	// +optional
	Detail string `json:"detail,omitempty"`
	// +optional
	StartedTime *metav1.Time `json:"startedTime,omitempty"`
	// +optional
	CompletedTime *metav1.Time `json:"completedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=aseat
// +kubebuilder:printcolumn:name="Seat",type=string,JSONPath=`.spec.seatKey`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.executionState`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentSeat is a controller-owned persistent seat.
type AgentSeat struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSeatSpec   `json:"spec,omitempty"`
	Status AgentSeatStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AgentSeatList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSeat `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentOrganization{}, &AgentOrganizationList{}, &AgentSeat{}, &AgentSeatList{})
}
