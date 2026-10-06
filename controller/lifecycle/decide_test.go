package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

const rev = "sha256:new"

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := now.Add(-d); return &t }

func backend(replicas int32, pods ...seatruntime.PodStatus) *seatruntime.Status {
	return &seatruntime.Status{Exists: true, Replicas: replicas, Pods: pods}
}

func readyPod(uid string) seatruntime.PodStatus {
	return seatruntime.PodStatus{Name: "seat-0", UID: uid, Revision: rev, Phase: "Running", Ready: true, StartTime: ago(time.Hour)}
}

func rt(mut func(r *runtimeapi.SeatRuntime)) *runtimeapi.SeatRuntime {
	r := &runtimeapi.SeatRuntime{State: "Warm", LeaseGeneration: 4}
	if mut != nil {
		mut(r)
	}
	return r
}

func base(mut func(in *Input)) Input {
	in := Input{Now: now, IdlePolicy: PolicyWarmThenStop, IdleTimeout: 15 * time.Minute, ConfigRevision: rev}
	if mut != nil {
		mut(&in)
	}
	return in
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name     string
		in       Input
		replicas int32
		state    v1alpha1.ExecutionState
		reason   string
		delete   string
		fence    bool
		requeue  time.Duration // 0 = don't care; -1 = must be zero
	}{
		{
			name: "new seat warm_then_stop stays stopped", in: base(nil),
			replicas: 0, state: v1alpha1.StateStopped, reason: ReasonStopped,
		},
		{
			name: "new seat warm starts", in: base(func(in *Input) { in.IdlePolicy = PolicyWarm }),
			replicas: 1, state: v1alpha1.StateProvisioning, reason: ReasonStarting,
		},
		{
			name: "wake on pending deliveries (A07)",
			in: base(func(in *Input) {
				in.Backend = backend(0)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.PendingDeliveries = 2; r.State = "Stopped" })
			}),
			replicas: 1, state: v1alpha1.StateProvisioning, reason: ReasonStarting,
		},
		{
			name:     "wake for synthetic probe",
			in:       base(func(in *Input) { in.Backend = backend(0); in.ProbePending = true }),
			replicas: 1, state: v1alpha1.StateProvisioning,
		},
		{
			name: "stopped seat without work stays stopped",
			in: base(func(in *Input) {
				in.Backend = backend(0)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.State = "Stopped" })
			}),
			replicas: 0, state: v1alpha1.StateStopped,
		},
		{
			name: "warm and idle past timeout stops",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) {
					r.LastActivity = ago(20 * time.Minute)
					r.LeaseHolder = "p1"
					r.LeaseExpiresAt = now.Add(20 * time.Second)
				})
			}),
			replicas: 0, state: v1alpha1.StateQuiescing, reason: ReasonIdle,
		},
		{
			name: "warm within idle timeout keeps running and requeues at the deadline",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) {
					r.LastActivity = ago(10 * time.Minute)
					r.LeaseHolder = "p1"
					r.LeaseExpiresAt = now.Add(20 * time.Second)
				})
			}),
			replicas: 1, state: v1alpha1.StateWarm, reason: ReasonRunning, requeue: 5 * time.Minute,
		},
		{
			name: "idle falls back to pod start time without recorded activity",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(nil)
			}),
			replicas: 0, state: v1alpha1.StateQuiescing, reason: ReasonIdle,
		},
		{
			name: "executing seat is never idle-stopped",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.State = "Executing"; r.LastActivity = ago(time.Hour) })
			}),
			replicas: 1, state: v1alpha1.StateExecuting,
		},
		{
			name: "pending deliveries prevent idle stop",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.PendingDeliveries = 1; r.LastActivity = ago(time.Hour) })
			}),
			replicas: 1, state: v1alpha1.StateWarm,
		},
		{
			name: "pending probe prevents idle stop",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LastActivity = ago(time.Hour) })
				in.ProbePending = true
			}),
			replicas: 1, state: v1alpha1.StateWarm,
		},
		{
			name: "warm policy never idle-stops",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				in.Backend = backend(1, readyPod("p1"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LastActivity = ago(time.Hour) })
			}),
			replicas: 1, state: v1alpha1.StateWarm,
		},
		{
			name: "config revision change deletes the pod at a safe boundary (A13/A14)",
			in: base(func(in *Input) {
				p := readyPod("p1")
				p.Revision = "sha256:old"
				in.Backend = backend(1, p)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.AdoptedRevision = "sha256:old"; r.LastActivity = ago(time.Minute) })
			}),
			replicas: 1, state: v1alpha1.StateQuiescing, reason: ReasonConfigChanged, delete: "seat-0",
		},
		{
			name: "pod already on the new revision is not deleted while the runner adopts it",
			in: base(func(in *Input) {
				in.Backend = backend(1, readyPod("p2"))
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.AdoptedRevision = "sha256:old"; r.LastActivity = ago(time.Minute) })
			}),
			replicas: 1, state: v1alpha1.StateWarm,
		},
		{
			name: "stopped seat with old revision is not woken for the change",
			in: base(func(in *Input) {
				in.Backend = backend(0)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.AdoptedRevision = "sha256:old"; r.State = "Stopped" })
			}),
			replicas: 0, state: v1alpha1.StateStopped,
		},
		{
			name: "terminating old pod after restart is quiescing",
			in: base(func(in *Input) {
				p := readyPod("p1")
				p.Terminating, p.Revision = true, "sha256:old"
				in.Backend = backend(1, p)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) {
					r.LeaseHolder, r.LeaseExpiresAt, r.LastActivity = "p1", now.Add(10*time.Second), ago(time.Minute)
				})
			}),
			replicas: 1, state: v1alpha1.StateQuiescing, reason: ReasonStopping,
		},
		{
			name: "admin suspension scales to zero and blocks",
			in: base(func(in *Input) {
				in.AdminSuspended = true
				in.Backend = backend(0)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.PendingDeliveries = 3 })
			}),
			replicas: 0, state: v1alpha1.StateBlocked, reason: ReasonAdminSuspended,
		},
		{
			name: "admin suspension of a running seat quiesces first",
			in: base(func(in *Input) {
				in.AdminSuspended = true
				in.Backend = backend(1, readyPod("p1"))
			}),
			replicas: 0, state: v1alpha1.StateQuiescing,
		},
		{
			name: "backend validation failure blocks without fallback (A16)",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				in.Blocked = seatruntime.Errorf(seatruntime.Misconfigured, "runtime_class", "missing")
			}),
			replicas: 0, state: v1alpha1.StateBlocked, reason: "Misconfigured",
		},
		{
			name:     "untyped blocking error",
			in:       base(func(in *Input) { in.Blocked = errors.New("boom") }),
			replicas: 0, state: v1alpha1.StateBlocked, reason: "BackendError",
		},
		{
			name:     "retired seat is stopped",
			in:       base(func(in *Input) { in.Retired = true; in.Backend = backend(0) }),
			replicas: 0, state: v1alpha1.StateRetired, reason: ReasonRetired,
		},
		{
			name:     "retiring seat waits for the pod",
			in:       base(func(in *Input) { in.Retired = true; in.Backend = backend(1, readyPod("p1")) }),
			replicas: 0, state: v1alpha1.StateQuiescing,
		},
		{
			name: "recovery: lease held by a vanished pod is fenced (A08)",
			in: base(func(in *Input) {
				in.Backend = backend(1)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) {
					r.LeaseHolder, r.LeaseExpiresAt, r.State = "dead-pod", now.Add(20*time.Second), "Executing"
				})
			}),
			replicas: 1, state: v1alpha1.StateRecovering, reason: ReasonFencing, fence: true,
		},
		{
			name: "recovery: replacement pod running while old lease is live is fenced",
			in: base(func(in *Input) {
				p := readyPod("new")
				p.Ready = false
				in.Backend = backend(1, p)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LeaseHolder, r.LeaseExpiresAt = "old", now.Add(20*time.Second) })
			}),
			replicas: 1, state: v1alpha1.StateRecovering, fence: true,
		},
		{
			name: "recovery: holder pod still present on an unreachable node is never fenced",
			in: base(func(in *Input) {
				p := readyPod("old")
				p.Phase, p.Ready = "Unknown", false
				in.Backend = backend(1, p)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LeaseHolder, r.LeaseExpiresAt = "old", now.Add(20*time.Second) })
			}),
			replicas: 1, state: v1alpha1.StateRecovering, reason: ReasonPodFailed,
		},
		{
			name: "recovery: terminating holder is waited for, not fenced",
			in: base(func(in *Input) {
				p := readyPod("old")
				p.Terminating = true
				in.Backend = backend(1, p)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LeaseHolder, r.LeaseExpiresAt = "old", now.Add(20*time.Second) })
			}),
			replicas: 1, state: v1alpha1.StateQuiescing,
		},
		{
			name: "expired lease needs no fence",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				in.Backend = backend(1)
				in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LeaseHolder, r.LeaseExpiresAt = "dead", now.Add(-time.Second) })
			}),
			replicas: 1, state: v1alpha1.StateProvisioning,
		},
		{
			name: "crash looping runner is recovering",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				p := readyPod("p1")
				p.Ready, p.Waiting = false, "CrashLoopBackOff"
				in.Backend = backend(1, p)
			}),
			replicas: 1, state: v1alpha1.StateRecovering, reason: ReasonPodFailed,
		},
		{
			name: "unpullable harness image",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				p := readyPod("p1")
				p.Ready, p.Waiting, p.Phase = false, "ImagePullBackOff", "Pending"
				in.Backend = backend(1, p)
			}),
			replicas: 1, state: v1alpha1.StateProvisioning, reason: ReasonImageUnavailable,
		},
		{
			name: "ready pod without runtime info is warm",
			in: base(func(in *Input) {
				in.IdlePolicy = PolicyWarm
				in.Backend = backend(1, readyPod("p1"))
			}),
			replicas: 1, state: v1alpha1.StateWarm, reason: ReasonRunning, requeue: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Decide(tc.in)
			if a.Replicas != tc.replicas {
				t.Errorf("replicas = %d, want %d", a.Replicas, tc.replicas)
			}
			if a.State != tc.state {
				t.Errorf("state = %s, want %s (%s: %s)", a.State, tc.state, a.Reason, a.Message)
			}
			if tc.reason != "" && a.Reason != tc.reason {
				t.Errorf("reason = %s, want %s", a.Reason, tc.reason)
			}
			if a.DeletePod != tc.delete {
				t.Errorf("delete = %q, want %q", a.DeletePod, tc.delete)
			}
			if a.Fence != tc.fence {
				t.Errorf("fence = %t, want %t", a.Fence, tc.fence)
			}
			if a.Fence && (a.FenceGeneration != 4 || a.FenceHolder == "") {
				t.Errorf("fence generation/holder = %d/%q", a.FenceGeneration, a.FenceHolder)
			}
			switch {
			case tc.requeue == -1 && a.RequeueAfter != 0:
				t.Errorf("requeue = %s, want none", a.RequeueAfter)
			case tc.requeue > 0 && a.RequeueAfter != tc.requeue:
				t.Errorf("requeue = %s, want %s", a.RequeueAfter, tc.requeue)
			}
		})
	}
}

func TestDecideIsDeterministic(t *testing.T) {
	in := base(func(in *Input) {
		in.Backend = backend(1, readyPod("p1"))
		in.Runtime = rt(func(r *runtimeapi.SeatRuntime) { r.LastActivity = ago(time.Minute) })
	})
	if a, b := Decide(in), Decide(in); a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
}
