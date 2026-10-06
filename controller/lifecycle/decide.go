// Package lifecycle holds the controller's seat lifecycle decision
// (contracts.md "Lifecycle decisions"). Decide is a pure function of the
// observed seat, backend and platform runtime state and the current time.
package lifecycle

import (
	"fmt"
	"time"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

// Idle policies.
const (
	PolicyWarm         = "warm"
	PolicyWarmThenStop = "warm_then_stop"
)

// Reasons reported with the execution state.
const (
	ReasonRetired           = "Retired"
	ReasonRetiring          = "Retiring"
	ReasonAdminSuspended    = "AdminSuspended"
	ReasonPendingDeliveries = "PendingDeliveries"
	ReasonProbe             = "SyntheticProbe"
	ReasonIdle              = "IdleTimeout"
	ReasonConfigChanged     = "ConfigRevisionChanged"
	ReasonFencing           = "FencingPreviousExecution"
	ReasonPodFailed         = "PodFailed"
	ReasonStarting          = "Starting"
	ReasonStopping          = "Stopping"
	ReasonStopped           = "Stopped"
	ReasonRunning           = "Running"
	ReasonImageUnavailable  = "HarnessImageUnavailable"
)

// Input is everything Decide needs.
type Input struct {
	Now            time.Time
	Retired        bool
	AdminSuspended bool
	// Blocked is a backend validation failure; nil when the seat can run.
	Blocked        error
	IdlePolicy     string
	IdleTimeout    time.Duration
	ConfigRevision string
	// Backend is the observed backend state (nil before the first ensure).
	Backend *seatruntime.Status
	// Runtime is the platform's view of the seat (nil when unknown).
	Runtime *runtimeapi.SeatRuntime
	// ProbePending means a synthetic probe for this revision needs the seat running.
	ProbePending bool
}

// Action is the decision.
type Action struct {
	// Replicas is the desired replica count (0 or 1).
	Replicas int32
	// DeletePod names a Pod to delete gracefully (safe-boundary restart).
	DeletePod string
	// Fence requests POST /internal/v1/seats/{id}/fence with FenceGeneration,
	// once FenceHolder has been confirmed gone with a live read.
	Fence           bool
	FenceGeneration int64
	FenceHolder     string
	State           v1alpha1.ExecutionState
	Reason          string
	Message         string
	// RequeueAfter asks for re-evaluation (e.g. at the idle deadline).
	RequeueAfter time.Duration
}

func minRequeue(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// livePod returns the seat's non-terminating Pod, if any.
func livePod(st *seatruntime.Status) *seatruntime.PodStatus {
	if st == nil {
		return nil
	}
	for i := range st.Pods {
		if !st.Pods[i].Terminating {
			return &st.Pods[i]
		}
	}
	return nil
}

func podByUID(st *seatruntime.Status, uid string) *seatruntime.PodStatus {
	if st == nil {
		return nil
	}
	for i := range st.Pods {
		if st.Pods[i].UID == uid {
			return &st.Pods[i]
		}
	}
	return nil
}

func hasPods(st *seatruntime.Status) bool { return st != nil && len(st.Pods) > 0 }

// stopped is the state of a seat whose desired replicas are zero.
func stopped(a Action, st *seatruntime.Status, reason, msg string) Action {
	a.Replicas = 0
	if hasPods(st) {
		a.State, a.Reason, a.Message = v1alpha1.StateQuiescing, ReasonStopping, "waiting for the runner to quiesce and stop"
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
		return a
	}
	a.Reason, a.Message = reason, msg
	return a
}

// Decide returns the lifecycle action for a seat.
func Decide(in Input) Action {
	var a Action
	st := in.Backend
	switch {
	case in.Retired:
		a.State = v1alpha1.StateRetired
		return stopped(a, st, ReasonRetired, "seat retired; workspace retained")
	case in.Blocked != nil:
		a.State = v1alpha1.StateBlocked
		reason := string(seatruntime.KindOf(in.Blocked))
		if reason == "" {
			reason = "BackendError"
		}
		a = stopped(a, st, reason, in.Blocked.Error())
		if a.State != v1alpha1.StateQuiescing {
			a.State = v1alpha1.StateBlocked
		}
		a.RequeueAfter = minRequeue(a.RequeueAfter, 30*time.Second)
		return a
	case in.AdminSuspended:
		a.State = v1alpha1.StateBlocked
		a = stopped(a, st, ReasonAdminSuspended, "execution suspended by an administrator")
		if a.State == v1alpha1.StateQuiescing {
			return a
		}
		a.State = v1alpha1.StateBlocked
		return a
	}

	// Desired replicas.
	var current int32
	if st != nil && st.Exists {
		current = st.Replicas
	}
	rt := in.Runtime
	wakeReason := ""
	switch in.IdlePolicy {
	case PolicyWarm:
		a.Replicas = 1
	default: // warm_then_stop
		a.Replicas = current
		switch {
		case in.ProbePending:
			a.Replicas, wakeReason = 1, ReasonProbe
		case rt != nil && rt.PendingDeliveries > 0:
			a.Replicas, wakeReason = 1, ReasonPendingDeliveries
		case current == 1 && rt != nil && rt.State == string(v1alpha1.StateWarm) && in.IdleTimeout > 0 && runnerReady(st):
			last := rt.LastActivity
			if last == nil {
				if p := livePod(st); p != nil && p.StartTime != nil {
					last = p.StartTime
				}
			}
			if last != nil {
				deadline := last.Add(in.IdleTimeout)
				if !in.Now.Before(deadline) {
					a.Replicas = 0
					a.State = v1alpha1.StateQuiescing
					a.Reason, a.Message = ReasonIdle, fmt.Sprintf("idle since %s; stopping (warm_then_stop)", last.UTC().Format(time.RFC3339))
				} else {
					a.RequeueAfter = deadline.Sub(in.Now)
				}
			}
		}
	}

	// Recovery: a live lease held by a Pod that no longer runs the seat.
	if rt != nil && rt.LeaseHolder != "" && rt.LeaseExpiresAt.After(in.Now) {
		live := livePod(st)
		if live == nil || live.UID != rt.LeaseHolder {
			// A holder Pod that still exists (terminating, or on an unreachable
			// node) is never fenced or force-deleted: the runner releases the
			// lease when it stops, or the Pod object disappears once its node
			// is confirmed gone.
			if podByUID(st, rt.LeaseHolder) == nil {
				a.Fence, a.FenceGeneration, a.FenceHolder = true, rt.LeaseGeneration, rt.LeaseHolder
				a.State, a.Reason = v1alpha1.StateRecovering, ReasonFencing
				a.Message = fmt.Sprintf("lease generation %d is held by a Pod that no longer exists; fencing it before a new runner starts", rt.LeaseGeneration)
				a.RequeueAfter = minRequeue(a.RequeueAfter, 2*time.Second)
				return a
			}
		}
	}

	if a.Replicas == 0 {
		if a.State == v1alpha1.StateQuiescing && hasPods(st) {
			a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
			return a
		}
		a.State = v1alpha1.StateStopped
		r := a.RequeueAfter
		a = stopped(a, st, ReasonStopped, "compute removed; durable state retained")
		a.RequeueAfter = minRequeue(a.RequeueAfter, r)
		return a
	}

	pod := livePod(st)
	if pod != nil && pod.Revision != in.ConfigRevision {
		a.DeletePod = pod.Name
		a.State, a.Reason = v1alpha1.StateQuiescing, ReasonConfigChanged
		a.Message = fmt.Sprintf("restarting at a safe boundary to adopt revision %s", short(in.ConfigRevision))
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
		return a
	}
	switch {
	case pod == nil && hasPods(st):
		a.State, a.Reason, a.Message = v1alpha1.StateQuiescing, ReasonStopping, "previous Pod terminating"
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
	case pod == nil:
		a.State, a.Reason = v1alpha1.StateProvisioning, ReasonStarting
		a.Message = "starting runner"
		if wakeReason != "" {
			a.Message = "waking: " + wakeReason
		}
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
	case pod.Phase == "Failed" || pod.Phase == "Unknown" || pod.Waiting == "CrashLoopBackOff":
		a.State, a.Reason = v1alpha1.StateRecovering, ReasonPodFailed
		a.Message = fmt.Sprintf("Pod %s phase %s %s", pod.Name, pod.Phase, pod.Waiting)
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
	case pod.Waiting == "ErrImagePull" || pod.Waiting == "ImagePullBackOff" || pod.Waiting == "InvalidImageName":
		a.State, a.Reason = v1alpha1.StateProvisioning, ReasonImageUnavailable
		a.Message = fmt.Sprintf("harness image cannot be pulled (%s)", pod.Waiting)
		a.RequeueAfter = minRequeue(a.RequeueAfter, 10*time.Second)
	case !pod.Ready:
		a.State, a.Reason, a.Message = v1alpha1.StateProvisioning, ReasonStarting, "runner not ready"
		a.RequeueAfter = minRequeue(a.RequeueAfter, 5*time.Second)
	default:
		a.State, a.Reason, a.Message = v1alpha1.StateWarm, ReasonRunning, "runner ready"
		if rt != nil {
			switch s := v1alpha1.ExecutionState(rt.State); s {
			case v1alpha1.StateWarm, v1alpha1.StateExecuting, v1alpha1.StateQuiescing:
				a.State = s
			}
		}
	}
	return a
}

func short(rev string) string {
	if len(rev) > 19 {
		return rev[:19]
	}
	return rev
}

// runnerReady reports whether the seat's live Pod is ready; idle decisions are
// only taken for a healthy, idle runner.
func runnerReady(st *seatruntime.Status) bool {
	p := livePod(st)
	return p != nil && p.Ready
}
