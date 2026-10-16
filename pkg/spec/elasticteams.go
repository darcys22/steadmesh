package spec

// ElasticTeams opts selected durable seats into bounded, execution-scoped teams.
// Workers cannot launch nested workers. Removing the policy disables new launches.
type ElasticTeams struct {
	// +kubebuilder:validation:MinItems=1
	EnabledSeats []string `json:"enabled_seats"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxActiveWorkers int `json:"max_active_workers"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	MaxActivePerExecution int `json:"max_active_per_execution"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	MaxLaunchesPerExecution int `json:"max_launches_per_execution"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1200
	MaxTimeoutSeconds int `json:"max_timeout_seconds"`
	// Draining prevents new launches while existing workers finish.
	// +optional
	Draining bool `json:"draining,omitempty"`
	// +kubebuilder:validation:MinProperties=1
	Templates map[string]WorkerTemplate `json:"templates"`
}

// WorkerTemplate fixes a role and a read-only subset of the requester's tools.
// Optional profile selectors assert the enabled requester's profiles; workers inherit
// its pinned runtime resources, model and authorization rather than escalating.
type WorkerTemplate struct {
	// Instructions is nonempty and limited to 8192 UTF-8 bytes by the compiler.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=8192
	Instructions string `json:"instructions"`
	// +optional
	// +kubebuilder:validation:MaxItems=7
	ToolNames []string `json:"tool_names,omitempty"`
	// +optional
	HarnessProfile string `json:"harness_profile,omitempty"`
	// +optional
	ExecutionProfile string `json:"execution_profile,omitempty"`
}
