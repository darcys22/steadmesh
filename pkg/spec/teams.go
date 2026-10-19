package spec

// TeamCoordination controls collaboration independently of membership and authority.
// No role name, editor selector or coordination mode grants tools or message routes.
type TeamCoordination struct {
	// +optional
	// +kubebuilder:validation:Enum=declared;self_organising
	Mode string `json:"mode,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	Mission string `json:"mission,omitempty"`
	// WorkStore is an explicitly granted shared work board.
	// +optional
	WorkStore string `json:"work_store,omitempty"`
	// MissionEditors are seat:<key> or team:<key> subjects, restricted to members.
	// An empty list grants no agent mission-edit permission.
	// +optional
	MissionEditors []string `json:"mission_editors,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=none;required
	ReviewPolicy string `json:"review_policy,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxMessagesPerExecution int `json:"max_messages_per_execution,omitempty"`
}

// TeamContext grants only self-context edits, independently from memory grants.
type TeamContext struct {
	// RequireInstructionApproval makes every instruction update a reviewed proposal.
	// Across memberships, any approval requirement wins over direct self-edit.
	// +optional
	RequireInstructionApproval bool `json:"require_instruction_approval,omitempty"`
	// InstructionApprovers are explicit seat:<member-key> or team:<this-team> subjects.
	// +optional
	InstructionApprovers []string `json:"instruction_approvers,omitempty"`
	// +optional
	EditableInstructions bool `json:"editable_instructions,omitempty"`
	// +optional
	EditableSummary bool `json:"editable_summary,omitempty"`
}

// TeamPool declares admission and lifecycle bounds, never runtime membership.
// Members receive independent ordinary seat identities from explicit templates.
type TeamPool struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1024
	MinMembers int `json:"min_members,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxMembers int `json:"max_members"`
	// +kubebuilder:validation:MinProperties=1
	Templates map[string]WorkerTemplate `json:"templates"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	DrainTimeoutSeconds int `json:"drain_timeout_seconds,omitempty"`
	// +optional
	// RetentionSeconds defaults to zero (retain indefinitely). A positive value
	// explicitly authorizes cleanup of private retired-member data after this interval.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2592000
	RetentionSeconds int `json:"retention_seconds,omitempty"`
	// +optional
	Draining bool `json:"draining,omitempty"`
}
