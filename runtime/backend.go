// Package runtime defines the sandbox backend contract (design §11.3).
//
// A backend provisions and operates the execution environment of one seat:
// capability discovery, validation, provisioning with a desired replica count
// of 0 or 1, status, quiescing and stopping, and cleanup with retention.
// Backends never fall back to a weaker runtime: a requested feature that is
// unsupported, misconfigured or temporarily unavailable is reported as a typed
// *Error and nothing is started (A16).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/pkg/compile"
)

// ErrorKind classifies why a requested feature cannot be provided (§11.3).
type ErrorKind string

const (
	// Unsupported: the backend cannot provide the feature at all.
	Unsupported ErrorKind = "Unsupported"
	// Misconfigured: the feature could be provided but the cluster or
	// declaration is wrong (e.g. a RuntimeClass that does not exist).
	Misconfigured ErrorKind = "Misconfigured"
	// Unavailable: a dependency is temporarily unavailable; retry later.
	Unavailable ErrorKind = "Unavailable"
)

// Error is a typed backend failure. It never contains secret values.
type Error struct {
	Kind    ErrorKind
	Feature string
	Message string
}

func (e *Error) Error() string {
	if e.Feature == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Kind, e.Feature, e.Message)
}

// Errorf builds a typed backend error.
func Errorf(kind ErrorKind, feature, format string, args ...any) *Error {
	return &Error{Kind: kind, Feature: feature, Message: fmt.Sprintf(format, args...)}
}

// KindOf returns the kind of a typed backend error, or "" for other errors.
func KindOf(err error) ErrorKind {
	var be *Error
	if errors.As(err, &be) {
		return be.Kind
	}
	return ""
}

// Retention selects what cleanup keeps.
type Retention string

const (
	// RetainData removes the runtime and keeps the workspace volume.
	RetainData Retention = "retain"
	// DeleteData removes the runtime and the workspace volume.
	DeleteData Retention = "delete"
)

// Seat is the backend's view of one seat.
type Seat struct {
	Namespace string
	// Name is the deterministic base name (pkg/names.Seat).
	Name           string
	OrgKey         string
	OrgID          string
	SeatKey        string
	SeatID         string
	ConfigRevision string
	// Manifest is the effective seat manifest (harness, execution and sandbox profiles).
	Manifest compile.SeatManifest
	// Owner, if set, owns every seat object except the workspace volume.
	Owner *metav1.OwnerReference
	// AdoptFrom is the retired seat ID whose retained workspace may be adopted.
	AdoptFrom string
}

// Capabilities describes what a backend supports.
type Capabilities struct {
	Backend  string
	Features []string
	// Enforcement lists the sandbox enforcement features the backend can provide.
	Enforcement []string
}

// PodStatus is the observed state of one seat Pod.
type PodStatus struct {
	Name        string
	UID         string
	Revision    string
	Phase       string
	Ready       bool
	Terminating bool
	// Waiting is the container waiting reason (e.g. CrashLoopBackOff, ImagePullBackOff).
	Waiting   string
	StartTime *time.Time
	NodeName  string
}

// Status is the observed state of a seat's backend instance.
type Status struct {
	// BackendRef identifies the backend instance, e.g. statefulset/<name>.
	BackendRef string
	Exists     bool
	// Replicas is the desired replica count currently set on the backend.
	Replicas int32
	Pods     []PodStatus
	// StorageHandle is the workspace volume claim.
	StorageHandle string
	StorageExists bool
	StoragePhase  string
	// ManifestRevision is the config revision of the published manifest, if any.
	ManifestRevision string
}

// EnforcementProbe asks the backend to verify that the seat network policy is
// actually enforced in a namespace for a sandbox profile.
type EnforcementProbe struct {
	Namespace string
	// ProfileDigest identifies the sandbox profile contents.
	ProfileDigest string
	// Seat is a seat using the profile; the probe runs under its policy.
	Seat *Seat
	// Nonce forces a fresh probe when it differs from the cached result's nonce.
	Nonce string
}

// ProbeState is the outcome of an enforcement probe.
type ProbeState string

const (
	ProbePending ProbeState = "pending"
	ProbePassed  ProbeState = "passed"
	ProbeFailed  ProbeState = "failed"
)

// ProbeResult is a cached enforcement probe result.
type ProbeResult struct {
	State  ProbeState
	Detail string
	Nonce  string
	Time   time.Time
}

// Backend is the sandbox backend contract (§11.3).
type Backend interface {
	// Capabilities reports the backend's supported features.
	Capabilities(ctx context.Context) Capabilities
	// Validate checks that the seat's requested runtime, enforcement and
	// lifecycle features are available. It returns a typed *Error otherwise.
	Validate(ctx context.Context, s *Seat) error
	// PublishManifest writes the seat's read-only manifest files.
	PublishManifest(ctx context.Context, s *Seat, files map[string]string) error
	// Ensure validates the seat and converges its objects with the desired
	// replica count (0 or 1). On validation failure it creates no workload,
	// scales an existing one to zero and returns the typed error.
	Ensure(ctx context.Context, s *Seat, replicas int32) (*Status, error)
	// Status observes the seat's backend instance.
	Status(ctx context.Context, s *Seat) (*Status, error)
	// Quiesce asks the running Pod to stop at a safe boundary (graceful
	// termination; the runner checkpoints on SIGTERM). It never force-deletes.
	Quiesce(ctx context.Context, s *Seat, pod string) error
	// Stop sets the desired replicas to zero.
	Stop(ctx context.Context, s *Seat) error
	// PodGone confirms with a live read that no Pod with the UID exists.
	PodGone(ctx context.Context, s *Seat, uid string) (bool, error)
	// Cleanup removes the runtime objects. With RetainData the workspace
	// volume is kept. It returns done=false while removal is in progress.
	Cleanup(ctx context.Context, s *Seat, retention Retention) (done bool, err error)
	// VerifyEnforcement runs (or returns the cached result of) the network
	// policy enforcement probe.
	VerifyEnforcement(ctx context.Context, p EnforcementProbe) (ProbeResult, error)
}
