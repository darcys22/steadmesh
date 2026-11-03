package spec

// StartupFallbackPolicy approves an ordered, bounded pre-start chain. It never
// authorizes switching after a native session starts or replaying a delivery.
// The primary and alternatives must select the same operator-provided image.
type StartupFallbackPolicy struct {
	// Profiles lists one to seven existing profiles. Nested fallback is forbidden.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	Profiles []string `json:"profiles"`
	// MaxAttempts includes the primary, and cannot exceed the chain length.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxAttempts int `json:"max_attempts"`
	// TotalTimeout is a required positive Go duration, at most 5m, shared by
	// the entire startup chain and bounded further by the parent context.
	TotalTimeout string `json:"total_timeout"`
}
