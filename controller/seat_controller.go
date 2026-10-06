package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller/lifecycle"
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/controller/readiness"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// SeatReconciler operates one seat's backend and lifecycle.
type SeatReconciler struct {
	Client   client.Client
	Backend  seatruntime.Backend
	Platform platform.API
	Pollers  *Pollers
	Recorder recorder.EventRecorder
	Now      func() time.Time
	// ProbeTimeout fails a synthetic probe that has not completed.
	ProbeTimeout time.Duration
	// ProbeRetry is the delay before a failed probe is retried.
	ProbeRetry time.Duration
}

func (r *SeatReconciler) event(obj client.Object, typ, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, typ, reason, action, note, args...)
	}
}

func (r *SeatReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var seat v1alpha1.AgentSeat
	if err := r.Client.Get(ctx, req.NamespacedName, &seat); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("organization", seat.Labels[v1alpha1.LabelOrganization], "organizationID", seat.Spec.OrganizationID,
		"seat", seat.Spec.SeatKey, "seatID", seat.Spec.SeatID)
	ctx = logf.IntoContext(ctx, log)
	orig := seat.DeepCopy()

	if seat.DeletionTimestamp != nil || seat.Spec.Retired {
		return r.retire(ctx, &seat, orig)
	}
	if controllerutil.AddFinalizer(&seat, v1alpha1.FinalizerSeat) {
		if err := r.Client.Update(ctx, &seat); err != nil {
			return ctrl.Result{}, err
		}
		orig = seat.DeepCopy()
	}
	now := r.Now()

	sm, err := r.manifest(ctx, &seat)
	if err != nil {
		seat.Status.ExecutionState = v1alpha1.StateProvisioning
		seat.Status.LastError = err.Error()
		setSeatCond(&seat, readiness.Condition(readiness.SeatReady, false, "ManifestPending", err.Error(), seat.Generation))
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.writeStatus(ctx, orig, &seat)
	}
	s := backendSeat(&seat, *sm)
	nonce := seat.Annotations[AnnotationSeatVerifyRequest]

	validateErr := r.Backend.Validate(ctx, s)
	blocked := validateErr
	enf := seatruntime.ProbeResult{}
	if validateErr == nil {
		enf, err = r.Backend.VerifyEnforcement(ctx, seatruntime.EnforcementProbe{
			Namespace: seat.Namespace, ProfileDigest: kube.ProfileDigest(sm.Sandbox), Seat: s, Nonce: nonce,
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("network policy probe: %w", err)
		}
		switch enf.State {
		case seatruntime.ProbePending:
			blocked = seatruntime.Errorf(seatruntime.Unavailable, "network_policy", "verifying NetworkPolicy enforcement before starting the seat: %s", enf.Detail)
		case seatruntime.ProbeFailed:
			blocked = seatruntime.Errorf(seatruntime.Misconfigured, "network_policy", "%s", enf.Detail)
		}
	}

	rt, _ := r.Pollers.Get(seat.Spec.OrganizationID, seat.Spec.SeatKey)
	st, err := r.Backend.Status(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}

	rev := seat.Spec.ConfigRevision
	probe := seat.Status.Probe
	eligible := blocked == nil && !seat.Spec.AdminSuspended
	current := readiness.ProbeCurrent(probe, rev, nonce)
	retryFailed := current && probe.Status == readiness.ProbeFailed &&
		(probe.CompletedTime == nil || now.Sub(probe.CompletedTime.Time) >= r.ProbeRetry)
	needProbe := eligible && (!current || retryFailed)
	probeActive := eligible && current && probe.Status == readiness.ProbePending

	in := lifecycle.Input{
		Now: now, AdminSuspended: seat.Spec.AdminSuspended, Blocked: blocked,
		IdlePolicy: sm.Execution.IdlePolicy, IdleTimeout: idleTimeout(sm.Execution.IdleTimeout),
		ConfigRevision: rev, Backend: st, Runtime: rt, ProbePending: needProbe || probeActive,
	}
	action := lifecycle.Decide(in)

	if action.Fence {
		gone, err := r.Backend.PodGone(ctx, s, action.FenceHolder)
		switch {
		case err != nil:
			return ctrl.Result{}, err
		case !gone:
			action.Message = fmt.Sprintf("previous Pod %s still exists; waiting before fencing", action.FenceHolder)
		default:
			if err := r.Platform.Fence(ctx, seat.Spec.SeatID, action.FenceGeneration); err != nil {
				log.Error(err, "fence previous execution", "generation", action.FenceGeneration)
				seat.Status.LastError = "fence: " + err.Error()
			} else {
				log.Info("fenced previous execution", "generation", action.FenceGeneration, "holder", action.FenceHolder)
				r.event(&seat, corev1.EventTypeNormal, "Fenced", "Recover", "fenced lease generation %d held by vanished Pod %s", action.FenceGeneration, action.FenceHolder)
			}
		}
	}

	st, ensureErr := r.Backend.Ensure(ctx, s, action.Replicas)
	if ensureErr != nil {
		if seatruntime.KindOf(ensureErr) == "" {
			return ctrl.Result{}, ensureErr
		}
		if blocked == nil {
			blocked = ensureErr
			in.Blocked, in.Backend, in.ProbePending = ensureErr, st, false
			action = lifecycle.Decide(in)
			eligible, needProbe, probeActive = false, false, false
		}
	}
	if blocked != nil && orig.Status.ExecutionState != v1alpha1.StateBlocked && action.State == v1alpha1.StateBlocked {
		r.event(&seat, corev1.EventTypeWarning, string(seatruntime.KindOf(blocked)), "Validate", "%s", blocked.Error())
	}
	if action.DeletePod != "" {
		if err := r.Backend.Quiesce(ctx, s, action.DeletePod); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("restarting seat at a safe boundary for a new config revision", "pod", action.DeletePod, "revision", rev)
		r.event(&seat, corev1.EventTypeNormal, lifecycle.ReasonConfigChanged, "Restart", "replacing Pod %s to adopt revision %s", action.DeletePod, rev)
	}
	if orig.Status.ExecutionState != action.State {
		log.Info("execution state", "from", orig.Status.ExecutionState, "to", action.State, "reason", action.Reason)
	}

	requeue := action.RequeueAfter
	if enf.State == seatruntime.ProbePending && validateErr == nil {
		// The probe Pod carries only one seat's labels; poll so every seat sees the result.
		requeue = minDuration(requeue, 2*time.Second)
	}
	probeErr := r.runProbe(ctx, &seat, st, now, nonce, needProbe, probeActive)
	if p := seat.Status.Probe; p != nil && p.Status == readiness.ProbePending && eligible {
		requeue = minDuration(requeue, 3*time.Second)
	}
	if retryFailed || (seat.Status.Probe != nil && seat.Status.Probe.Status == readiness.ProbeFailed) {
		requeue = minDuration(requeue, r.ProbeRetry)
	}

	r.fillStatus(&seat, st, rt, action, blocked, enf)
	switch {
	case probeErr != nil:
		seat.Status.LastError = "probe: " + probeErr.Error()
		requeue = minDuration(requeue, 5*time.Second)
	case blocked != nil:
		seat.Status.LastError = blocked.Error()
	case seat.Status.LastError != "" && !action.Fence:
		seat.Status.LastError = ""
	}
	return ctrl.Result{RequeueAfter: requeue}, r.writeStatus(ctx, orig, &seat)
}

func minDuration(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// manifest reads the published manifest of the seat's current revision.
func (r *SeatReconciler) manifest(ctx context.Context, seat *v1alpha1.AgentSeat) (*compile.SeatManifest, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: seat.Namespace, Name: seat.Spec.ManifestConfigMap}
	if err := r.Client.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("waiting for manifest ConfigMap %s", key.Name)
		}
		return nil, err
	}
	if got := cm.Annotations[kube.AnnotationConfigRevision]; got != seat.Spec.ConfigRevision {
		return nil, fmt.Errorf("waiting for manifest revision %s (published %s)", seat.Spec.ConfigRevision, got)
	}
	sm, err := parseManifest(cm.Data[kube.ManifestFile])
	if err != nil {
		return nil, err
	}
	return &sm, nil
}

// runProbe starts or polls the seat's synthetic probe for its revision (§5.4).
func (r *SeatReconciler) runProbe(ctx context.Context, seat *v1alpha1.AgentSeat, st *seatruntime.Status, now time.Time, nonce string, need, active bool) error {
	rev := seat.Spec.ConfigRevision
	switch {
	case need:
		// Only probe the runner that executes this revision: wait until a Pod
		// on an older revision has been replaced.
		if st != nil {
			for _, p := range st.Pods {
				if p.Revision != rev {
					return nil
				}
			}
		}
		resp, err := r.Platform.StartProbe(ctx, seat.Spec.SeatID)
		if err != nil {
			return err
		}
		t := metav1.NewTime(now.UTC().Truncate(time.Second))
		seat.Status.Probe = &v1alpha1.SeatProbeStatus{ID: resp.ProbeID, Revision: rev, Nonce: nonce, Status: readiness.ProbePending, StartedTime: &t}
		applyProbe(seat.Status.Probe, resp, now)
		logf.FromContext(ctx).Info("started synthetic probe", "probeID", resp.ProbeID, "revision", rev)
	case active:
		p := seat.Status.Probe
		resp, err := r.Platform.GetProbe(ctx, seat.Spec.SeatID, p.ID)
		if err != nil {
			if platform.IsNotFound(err) {
				p.Status, p.Detail = readiness.ProbeFailed, "probe no longer known to the platform"
				t := metav1.NewTime(now.UTC().Truncate(time.Second))
				p.CompletedTime = &t
				return nil
			}
			return err
		}
		applyProbe(p, resp, now)
		if p.Status == readiness.ProbePending && p.StartedTime != nil && now.Sub(p.StartedTime.Time) > r.ProbeTimeout {
			p.Status, p.Detail = readiness.ProbeFailed, fmt.Sprintf("probe did not complete within %s", r.ProbeTimeout)
			t := metav1.NewTime(now.UTC().Truncate(time.Second))
			p.CompletedTime = &t
		}
	default:
		return nil
	}
	if p := seat.Status.Probe; p.Status != readiness.ProbePending {
		typ := corev1.EventTypeNormal
		if p.Status == readiness.ProbeFailed {
			typ = corev1.EventTypeWarning
		}
		r.event(seat, typ, "Probe"+capitalise(p.Status), "Probe", "synthetic probe %s for revision %s: %s %s", p.ID, rev, p.Status, p.Detail)
	}
	return nil
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

func applyProbe(p *v1alpha1.SeatProbeStatus, resp *runtimeapi.ProbeResponse, now time.Time) {
	switch resp.Status {
	case readiness.ProbePassed, readiness.ProbeFailed:
		p.Status = resp.Status
		p.Detail = probeDetail(resp)
		t := metav1.NewTime(now.UTC().Truncate(time.Second))
		p.CompletedTime = &t
	default:
		p.Status = readiness.ProbePending
	}
}

func probeDetail(resp *runtimeapi.ProbeResponse) string {
	d := resp.Error
	for _, k := range sortedKeys(resp.Checks) {
		if d != "" {
			d += "; "
		}
		d += k + "=" + resp.Checks[k]
	}
	if len(d) > 512 {
		d = d[:509] + "..."
	}
	return d
}

func (r *SeatReconciler) fillStatus(seat *v1alpha1.AgentSeat, st *seatruntime.Status, rt *runtimeapi.SeatRuntime, a lifecycle.Action, blocked error, enf seatruntime.ProbeResult) {
	gen := seat.Generation
	seat.Status.ExecutionState = a.State
	if st != nil {
		seat.Status.BackendRef = st.BackendRef
	}
	if rt != nil {
		seat.Status.LeaseGeneration = rt.LeaseGeneration
		seat.Status.AdoptedRevision = rt.AdoptedRevision
		seat.Status.LatestCheckpoint = rt.LatestCheckpoint
		if rt.LastActivity != nil {
			seat.Status.LastActivityTime = timePtr(rt.LastActivity)
		}
	}
	switch {
	case st != nil && st.StorageExists && st.StoragePhase != string(corev1.ClaimLost):
		phase := st.StoragePhase
		if phase == "" {
			phase = "Pending"
		}
		setSeatCond(seat, readiness.Condition(readiness.SeatStorageReady, true, "Provisioned", fmt.Sprintf("workspace claim %s (%s)", st.StorageHandle, phase), gen))
	case st != nil && st.StorageExists:
		setSeatCond(seat, readiness.Condition(readiness.SeatStorageReady, false, "ClaimLost", "workspace claim "+st.StorageHandle+" lost its volume", gen))
	default:
		msg := "workspace claim not created"
		if blocked != nil && seatruntime.KindOf(blocked) != "" {
			msg = blocked.Error()
		}
		setSeatCond(seat, readiness.Condition(readiness.SeatStorageReady, false, "ClaimMissing", msg, gen))
	}
	switch {
	case blocked != nil:
		reason := string(seatruntime.KindOf(blocked))
		if reason == "" {
			reason = "BackendError"
		}
		msg := blocked.Error()
		var be *seatruntime.Error
		if errors.As(blocked, &be) {
			msg = be.Feature + ": " + be.Message
		}
		setSeatCond(seat, readiness.Condition(readiness.SeatBackendValid, false, reason, msg, gen))
	default:
		setSeatCond(seat, readiness.Condition(readiness.SeatBackendValid, true, "Enforced", "sandbox validated; "+enf.Detail, gen))
	}
	switch {
	case a.Reason == lifecycle.ReasonImageUnavailable:
		setSeatCond(seat, readiness.Condition(readiness.SeatHarnessReady, false, a.Reason, a.Message, gen))
	case a.State == v1alpha1.StateWarm || a.State == v1alpha1.StateExecuting:
		setSeatCond(seat, readiness.Condition(readiness.SeatHarnessReady, true, "Running", "harness runner ready", gen))
	default:
		if c := meta.FindStatusCondition(seat.Status.Conditions, readiness.SeatHarnessReady); c == nil || c.Status == metav1.ConditionFalse {
			setSeatCond(seat, readiness.Unknown(readiness.SeatHarnessReady, "NotRunning", "harness has not run this revision yet", gen))
		}
	}
	p := seat.Status.Probe
	nonce := seat.Annotations[AnnotationSeatVerifyRequest]
	switch {
	case a.State == v1alpha1.StateBlocked:
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, a.Reason, a.Message, gen))
	case !readiness.ProbeCurrent(p, seat.Spec.ConfigRevision, nonce):
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, "ProbePending", "synthetic probe for the current revision not run yet", gen))
	case p.Status == readiness.ProbePassed:
		// A stopped seat whose probe passed can wake and execute (§5.4).
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, true, "ProbePassed", "synthetic probe "+p.ID+" passed", gen))
	case p.Status == readiness.ProbeFailed:
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, "ProbeFailed", p.Detail, gen))
	default:
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, "ProbePending", "synthetic probe "+p.ID+" pending", gen))
	}
}

func setSeatCond(seat *v1alpha1.AgentSeat, c metav1.Condition) {
	meta.SetStatusCondition(&seat.Status.Conditions, c)
}

func (r *SeatReconciler) writeStatus(ctx context.Context, orig, seat *v1alpha1.AgentSeat) error {
	seat.Status.ObservedGeneration = seat.Generation
	if equality.Semantic.DeepEqual(orig.Status, seat.Status) {
		return nil
	}
	return r.Client.Status().Update(ctx, seat)
}

// retire stops and removes the seat runtime, keeping the workspace volume
// (retirement, §5.5). On deletion it then releases the finalizer.
func (r *SeatReconciler) retire(ctx context.Context, seat, orig *v1alpha1.AgentSeat) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	s := backendSeat(seat, compile.SeatManifest{})
	if s.OrgKey == "" {
		return ctrl.Result{}, errors.New("seat lacks organisation label")
	}
	done, err := r.Backend.Cleanup(ctx, s, seatruntime.RetainData)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		seat.Status.ExecutionState = v1alpha1.StateQuiescing
		setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, lifecycle.ReasonRetiring, "waiting for the runner to quiesce", seat.Generation))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.writeStatus(ctx, orig, seat)
	}
	if seat.DeletionTimestamp != nil {
		if controllerutil.RemoveFinalizer(seat, v1alpha1.FinalizerSeat) {
			log.Info("seat runtime removed; workspace retained", "claim", names.WorkspaceClaim(seat.Name))
			return ctrl.Result{}, r.Client.Update(ctx, seat)
		}
		return ctrl.Result{}, nil
	}
	if orig.Status.ExecutionState != v1alpha1.StateRetired {
		log.Info("seat retired; workspace retained", "claim", names.WorkspaceClaim(seat.Name))
		r.event(seat, corev1.EventTypeNormal, lifecycle.ReasonRetired, "Retire", "seat retired; workspace %s retained", names.WorkspaceClaim(seat.Name))
	}
	seat.Status.ExecutionState = v1alpha1.StateRetired
	seat.Status.BackendRef = ""
	setSeatCond(seat, readiness.Condition(readiness.SeatReady, false, lifecycle.ReasonRetired, "seat retired; workspace retained", seat.Generation))
	return ctrl.Result{}, r.writeStatus(ctx, orig, seat)
}
