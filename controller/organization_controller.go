package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/controller/readiness"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// OrganizationReconciler compiles an AgentOrganization, establishes durable
// identities through the platform, materialises AgentSeats and reports
// readiness (contracts.md "Readiness flow").
type OrganizationReconciler struct {
	Client       client.Client
	Platform     platform.API
	Backend      seatruntime.Backend
	Pollers      *Pollers
	Instructions *InstructionResolver
	Recorder     recorder.EventRecorder
	Catalog      compile.Catalog
	PlatformURL  string
	Now          func() time.Time

	// VerifyInterval is how long a passing platform verification is reused.
	VerifyInterval time.Duration
	// VerifyRetry is how long a failing verification is reused.
	VerifyRetry time.Duration
	// SyncInterval is how long a successful identity sync of an unchanged
	// manifest is reused.
	SyncInterval time.Duration

	mu     sync.Mutex
	syncs  map[types.UID]syncEntry
	verify map[types.UID]verifyEntry
}

type syncEntry struct {
	digest string
	at     time.Time
	resp   *runtimeapi.SyncResponse
}

type verifyEntry struct {
	generation int64
	nonce      string
	at         time.Time
	resp       *runtimeapi.VerifyResponse
	err        error
}

func (r *OrganizationReconciler) event(obj client.Object, typ, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, typ, reason, action, note, args...)
	}
}

func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var org v1alpha1.AgentOrganization
	if err := r.Client.Get(ctx, req.NamespacedName, &org); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("organization", org.Spec.Key, "organizationID", org.Status.OrganizationID)
	ctx = logf.IntoContext(ctx, log)

	if org.DeletionTimestamp != nil {
		return r.finalize(ctx, &org)
	}
	if !controllerutil.ContainsFinalizer(&org, v1alpha1.FinalizerOrganization) {
		patch := client.MergeFrom(org.DeepCopy())
		controllerutil.AddFinalizer(&org, v1alpha1.FinalizerOrganization)
		if err := r.Client.Patch(ctx, &org, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	orig := org.DeepCopy()
	gen := org.Generation
	nonce := org.Annotations[v1alpha1.AnnotationVerifyRequest]
	st := &org.Status

	// 1. Compile.
	m, err := compile.Compile(org.Spec.OrganizationSpec, r.Catalog)
	if err != nil {
		msg := err.Error()
		if orig.Status.ObservedGeneration != gen || !condIs(orig.Status.Conditions, v1alpha1.CondConfigured, metav1.ConditionFalse) {
			r.event(&org, corev1.EventTypeWarning, "InvalidSpec", "Compile", "%s", truncate(msg, 900))
		}
		log.Info("specification rejected", "errors", msg)
		r.setBlockedFrom(st, readiness.Condition(v1alpha1.CondConfigured, false, "InvalidSpec", msg, gen), gen)
		return r.finish(ctx, orig, &org, nonce, true, 0)
	}

	texts, ierrs := r.Instructions.ResolveAll(ctx, org.Namespace, m)
	if len(ierrs) > 0 {
		msgs := make([]string, len(ierrs))
		for i, e := range ierrs {
			msgs[i] = e.Error()
		}
		reason := instructionReason(ierrs)
		r.event(&org, corev1.EventTypeWarning, reason, "ResolveInstructions", "%s", truncate(strings.Join(msgs, "; "), 900))
		r.setBlockedFrom(st, readiness.Condition(v1alpha1.CondConfigured, false, reason, strings.Join(msgs, "; "), gen), gen)
		return r.finish(ctx, orig, &org, nonce, true, 30*time.Second)
	}
	setCond(st, readiness.Condition(v1alpha1.CondConfigured, true, "Compiled", fmt.Sprintf("%d seats resolved; defaults version %d", len(m.Seats), m.DefaultsVersion), gen))
	st.EffectiveRevision = m.Digest
	st.DefaultsVersion = m.DefaultsVersion

	// 2. Durable identities and policy activation.
	resp, err := r.sync(ctx, &org, m)
	if err != nil {
		log.Error(err, "platform sync")
		r.setBlockedFrom(st, readiness.Condition(v1alpha1.CondIdentitiesReady, false, "PlatformUnavailable", "organizations:sync: "+err.Error(), gen), gen)
		return r.finish(ctx, orig, &org, nonce, false, 10*time.Second)
	}
	st.OrganizationID = resp.OrganizationID
	r.Pollers.Ensure(resp.OrganizationID, m.Spec.Key, org.Namespace)
	var missing []string
	for _, k := range sortedKeys(m.Seats) {
		if id, ok := resp.Seats[k]; !ok || id.SeatID == "" {
			missing = append(missing, k)
		} else if sa := names.Seat(m.Spec.Key, k); id.ServiceAccount != "" && id.ServiceAccount != sa {
			missing = append(missing, fmt.Sprintf("%s (platform maps service account %q, expected %q)", k, id.ServiceAccount, sa))
		}
	}
	if len(missing) > 0 {
		r.setBlockedFrom(st, readiness.Condition(v1alpha1.CondIdentitiesReady, false, "IdentityMissing", "seats without durable identity: "+strings.Join(missing, ", "), gen), gen)
		return r.finish(ctx, orig, &org, nonce, false, 10*time.Second)
	}
	setCond(st, readiness.Condition(v1alpha1.CondIdentitiesReady, true, "Synced", fmt.Sprintf("%d seat identities active", len(resp.Seats)), gen))

	// 3. Materialise seats and their manifests; retire removed seats.
	views := make([]readiness.SeatView, 0, len(m.Seats))
	seatObjs := map[string]*v1alpha1.AgentSeat{}
	for _, k := range sortedKeys(m.Seats) {
		sm := m.Seats[k]
		seat, err := r.ensureSeat(ctx, &org, m, k, resp.Seats[k], texts, nonce)
		v := readiness.SeatView{Key: k, SandboxProfile: sm.SandboxProfile, ConfigRevision: sm.ConfigRevision, Nonce: nonce, Seat: seat, Ensure: err}
		if err != nil {
			if !apierrors.IsConflict(err) {
				log.Error(err, "ensure seat", "seat", k)
			}
			v.Seat = nil
		}
		seatObjs[k] = seat
		views = append(views, v)
	}
	if err := r.retireRemoved(ctx, &org, m); err != nil {
		return ctrl.Result{}, err
	}

	// 4. NetworkPolicy enforcement probe per sandbox profile in use.
	enforcement := map[string]seatruntime.ProbeResult{}
	for _, v := range views {
		if _, done := enforcement[v.SandboxProfile]; done || v.Seat == nil {
			continue
		}
		sm := m.Seats[v.Key]
		res, err := r.Backend.VerifyEnforcement(ctx, seatruntime.EnforcementProbe{
			Namespace: org.Namespace, ProfileDigest: kube.ProfileDigest(sm.Sandbox), Seat: backendSeat(v.Seat, sm), Nonce: nonce,
		})
		if err != nil {
			res = seatruntime.ProbeResult{State: seatruntime.ProbePending, Detail: err.Error()}
		}
		enforcement[v.SandboxProfile] = res
	}
	for _, sp := range sortedKeys(m.Spec.SandboxProfiles) {
		if _, ok := enforcement[sp]; !ok && profileInUse(m, sp) {
			enforcement[sp] = seatruntime.ProbeResult{State: seatruntime.ProbePending, Detail: "waiting for seats"}
		}
	}

	// 5. Connections, ingress and bindings. Only required connections
	// (compile.RequiredConnections) can block readiness.
	required := compile.RequiredConnections(m, compile.DefaultCatalog())
	vresp, verr, verifiedNow := r.verifyOrg(ctx, &org, gen, nonce)
	if verr != nil {
		for _, t := range []string{v1alpha1.CondConnectionsAuthenticated, v1alpha1.CondIngressReady, v1alpha1.CondBindingsValid} {
			setCond(st, readiness.Condition(t, false, "PlatformUnavailable", "verify: "+verr.Error(), gen))
		}
	} else {
		for _, c := range readiness.VerifyConditions(vresp, required, gen) {
			setCond(st, c)
		}
		if verifiedNow {
			t := metav1.NewTime(r.Now().UTC().Truncate(time.Second))
			st.LastVerifiedTime = &t
		}
	}

	// 6 and 7. Seat-derived conditions (storage, harness, sandbox, probes) and the aggregate.
	for _, c := range readiness.SeatConditions(views, enforcement, gen) {
		setCond(st, c)
	}
	setCond(st, readiness.Aggregate(st.Conditions, gen))
	st.Seats = seatSummaries(m, seatObjs, nonce)
	st.ConnectionDetails = r.connectionDetails(&org, m, resp)

	complete := verr == nil
	if complete && nonce != "" {
		complete = verifyFresh(views, enforcement, nonce, r.verifyNonce(org.UID))
	}
	requeue := r.VerifyInterval
	if !condIs(st.Conditions, v1alpha1.CondOperationalReady, metav1.ConditionTrue) {
		requeue = 10 * time.Second
	}
	if c := meta.FindStatusCondition(st.Conditions, v1alpha1.CondOperationalReady); c != nil {
		prev := meta.FindStatusCondition(orig.Status.Conditions, v1alpha1.CondOperationalReady)
		if prev == nil || prev.Status != c.Status || prev.Reason != c.Reason {
			log.Info("readiness", "operationalReady", c.Status, "reason", c.Reason, "message", c.Message)
			typ := corev1.EventTypeNormal
			if c.Status != metav1.ConditionTrue {
				typ = corev1.EventTypeWarning
			}
			r.event(&org, typ, "OperationalReady"+string(c.Status), "Reconcile", "%s: %s", c.Reason, truncate(c.Message, 900))
		}
	}
	return r.finish(ctx, orig, &org, nonce, complete, requeue)
}

func profileInUse(m *compile.Manifest, sp string) bool {
	for _, s := range m.Seats {
		if s.SandboxProfile == sp {
			return true
		}
	}
	return false
}

// verifyFresh reports whether every check of this pass answered the nonce:
// the platform verification, the enforcement probes and each seat's probe
// (terminal, i.e. passed or failed).
func verifyFresh(views []readiness.SeatView, enf map[string]seatruntime.ProbeResult, nonce, verifyNonce string) bool {
	if verifyNonce != nonce {
		return false
	}
	for _, r := range enf {
		if r.Nonce != nonce || r.State == seatruntime.ProbePending {
			return false
		}
	}
	for _, v := range views {
		if v.Ensure != nil {
			if apierrors.IsConflict(v.Ensure) {
				return false // transient: the seat is re-ensured on the next pass
			}
			continue // reported as a blocking condition; nothing to wait for
		}
		if v.Seat == nil {
			return false
		}
		p := v.Seat.Status.Probe
		if blocked := meta.FindStatusCondition(v.Seat.Status.Conditions, readiness.SeatBackendValid); blocked != nil && blocked.Status == metav1.ConditionFalse {
			if blocked.Reason == string(seatruntime.Unavailable) {
				return false // a dependency (e.g. the enforcement probe) is still settling
			}
			continue
		}
		if v.Seat.Spec.AdminSuspended {
			continue
		}
		if !readiness.ProbeCurrent(p, v.ConfigRevision, nonce) || p.Status == readiness.ProbePending {
			return false
		}
	}
	return true
}

// setBlockedFrom sets cond and marks every later condition as blocked by it.
func (r *OrganizationReconciler) setBlockedFrom(st *v1alpha1.AgentOrganizationStatus, cond metav1.Condition, gen int64) {
	setCond(st, cond)
	after := false
	for _, t := range readiness.Order {
		if t == cond.Type {
			after = true
			continue
		}
		if after {
			setCond(st, readiness.Unknown(t, "Blocked", "blocked by "+cond.Type, gen))
		}
	}
	setCond(st, readiness.Aggregate(st.Conditions, gen))
}

func setCond(st *v1alpha1.AgentOrganizationStatus, c metav1.Condition) {
	meta.SetStatusCondition(&st.Conditions, c)
}

func condIs(conds []metav1.Condition, t string, s metav1.ConditionStatus) bool {
	c := meta.FindStatusCondition(conds, t)
	return c != nil && c.Status == s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// finish writes status (only when it changed) and echoes the verify nonce
// once the pass for it is complete.
func (r *OrganizationReconciler) finish(ctx context.Context, orig, org *v1alpha1.AgentOrganization, nonce string, complete bool, requeue time.Duration) (ctrl.Result, error) {
	org.Status.ObservedGeneration = org.Generation
	if !equality.Semantic.DeepEqual(orig.Status, org.Status) {
		if err := r.Client.Status().Patch(ctx, org, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if complete && nonce != "" && org.Annotations[v1alpha1.AnnotationVerifyObserved] != nonce {
		patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, v1alpha1.AnnotationVerifyObserved, nonce))
		if err := r.Client.Patch(ctx, org, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("verification complete", "nonce", nonce)
	}
	if !complete && nonce != "" && org.Annotations[v1alpha1.AnnotationVerifyObserved] != nonce {
		requeue = minDuration(requeue, 3*time.Second)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *OrganizationReconciler) sync(ctx context.Context, org *v1alpha1.AgentOrganization, m *compile.Manifest) (*runtimeapi.SyncResponse, error) {
	r.mu.Lock()
	e, ok := r.syncs[org.UID]
	r.mu.Unlock()
	if ok && e.digest == m.Digest && r.Now().Sub(e.at) < r.SyncInterval {
		return e.resp, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	resp, err := r.Platform.Sync(ctx, runtimeapi.SyncRequest{Namespace: org.Namespace, Key: m.Spec.Key, SourceUID: string(org.UID), Manifest: raw})
	if err != nil {
		return nil, err
	}
	if resp.OrganizationID == "" {
		return nil, errors.New("platform returned no organization id")
	}
	for key, rs := range resp.Retiring {
		logf.FromContext(ctx).Info("seat retiring", "seat", key, "seatID", rs.SeatID, "retireBy", rs.RetireBy)
	}
	r.mu.Lock()
	r.syncs[org.UID] = syncEntry{digest: m.Digest, at: r.Now(), resp: resp}
	r.mu.Unlock()
	return resp, nil
}

func (r *OrganizationReconciler) verifyNonce(uid types.UID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.verify[uid].nonce
}

// verifyOrg calls the platform verify endpoint, reusing a recent result for
// the same generation and nonce. A new nonce always forces a fresh call (A25).
func (r *OrganizationReconciler) verifyOrg(ctx context.Context, org *v1alpha1.AgentOrganization, gen int64, nonce string) (*runtimeapi.VerifyResponse, error, bool) {
	now := r.Now()
	r.mu.Lock()
	e, ok := r.verify[org.UID]
	r.mu.Unlock()
	if ok && e.generation == gen && e.nonce == nonce {
		ttl := r.VerifyInterval
		if e.err != nil || !allOK(e.resp) {
			ttl = r.VerifyRetry
		}
		if now.Sub(e.at) < ttl {
			return e.resp, e.err, false
		}
	}
	resp, err := r.Platform.Verify(ctx, org.Status.OrganizationID)
	if err != nil {
		logf.FromContext(ctx).Error(err, "platform verify")
	}
	r.mu.Lock()
	r.verify[org.UID] = verifyEntry{generation: gen, nonce: nonce, at: now, resp: resp, err: err}
	r.mu.Unlock()
	return resp, err, err == nil
}

func allOK(v *runtimeapi.VerifyResponse) bool {
	if v == nil {
		return false
	}
	for _, m := range []map[string]runtimeapi.CheckResult{v.Connections, v.Ingress, v.Bindings} {
		for _, c := range m {
			if !c.OK {
				return false
			}
		}
	}
	return true
}

func toolsOf(sm compile.SeatManifest) []v1alpha1.ToolCapability {
	out := make([]v1alpha1.ToolCapability, 0, len(sm.Capabilities))
	for _, c := range sm.Capabilities {
		out = append(out, v1alpha1.ToolCapability{Resource: c.Resource, Operations: c.Operations, Targets: c.Targets})
	}
	return out
}

// ensureSeat creates or updates the AgentSeat and publishes its manifest.
func (r *OrganizationReconciler) ensureSeat(ctx context.Context, org *v1alpha1.AgentOrganization, m *compile.Manifest, key string, id runtimeapi.SeatIdentity, texts map[string]string, nonce string) (*v1alpha1.AgentSeat, error) {
	sm := m.Seats[key]
	name := names.Seat(m.Spec.Key, key)
	seat := &v1alpha1.AgentSeat{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: org.Namespace}}
	res, err := controllerutil.CreateOrUpdate(ctx, r.Client, seat, func() error {
		if seat.Spec.SeatID != "" && seat.Spec.SeatID != id.SeatID {
			return fmt.Errorf("seat %s: durable identity changed from %s to %s; refusing to rebind", key, seat.Spec.SeatID, id.SeatID)
		}
		if seat.Labels == nil {
			seat.Labels = map[string]string{}
		}
		seat.Labels[v1alpha1.LabelOrganization] = m.Spec.Key
		seat.Labels[v1alpha1.LabelSeat] = key
		seat.Labels[v1alpha1.LabelComponent] = kube.ComponentSeat
		if nonce != "" {
			if seat.Annotations == nil {
				seat.Annotations = map[string]string{}
			}
			seat.Annotations[AnnotationSeatVerifyRequest] = nonce
		}
		controllerutil.AddFinalizer(seat, v1alpha1.FinalizerSeat)
		if err := controllerutil.SetControllerReference(org, seat, r.Client.Scheme()); err != nil {
			return err
		}
		seat.Spec.OrganizationName = org.Name
		seat.Spec.OrganizationID = org.Status.OrganizationID
		seat.Spec.SeatKey = key
		seat.Spec.SeatID = id.SeatID
		seat.Spec.ConfigRevision = sm.ConfigRevision
		seat.Spec.HarnessProfile = sm.HarnessProfile
		seat.Spec.ExecutionProfile = sm.ExecutionProfile
		seat.Spec.SandboxProfile = sm.SandboxProfile
		seat.Spec.Workspace = v1alpha1.WorkspaceRef{ClaimName: names.WorkspaceClaim(name), Shared: sm.Workspace.Shared}
		seat.Spec.Tools = toolsOf(sm)
		seat.Spec.ManifestConfigMap = names.ManifestConfigMap(name)
		seat.Spec.Retired = false
		// spec.adminSuspended is an infrastructure control and is never reset here.
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("seat "+string(res), "seat", key, "seatID", id.SeatID, "revision", sm.ConfigRevision)
	}
	files, err := manifestFiles(sm, texts)
	if err != nil {
		return nil, err
	}
	if err := r.Backend.PublishManifest(ctx, backendSeat(seat, sm), files); err != nil {
		return nil, fmt.Errorf("publish manifest: %w", err)
	}
	return seat, nil
}

func (r *OrganizationReconciler) listSeats(ctx context.Context, org *v1alpha1.AgentOrganization, orgKey string) ([]v1alpha1.AgentSeat, error) {
	var list v1alpha1.AgentSeatList
	if err := r.Client.List(ctx, &list, client.InNamespace(org.Namespace), client.MatchingLabels{v1alpha1.LabelOrganization: orgKey}); err != nil {
		return nil, err
	}
	var out []v1alpha1.AgentSeat
	for _, s := range list.Items {
		if metav1.IsControlledBy(&s, org) {
			out = append(out, s)
		}
	}
	return out, nil
}

// retireRemoved retires seats removed from the declaration. The platform sync
// has made the identity retiring: it takes no new messages and gets a bounded
// retirement turn. The AgentSeat is marked retired; the seat controller keeps
// it running while the platform reports it retiring, then stops and removes
// its runtime (workspace retained), and the AgentSeat is deleted.
func (r *OrganizationReconciler) retireRemoved(ctx context.Context, org *v1alpha1.AgentOrganization, m *compile.Manifest) error {
	seats, err := r.listSeats(ctx, org, m.Spec.Key)
	if err != nil {
		return err
	}
	for i := range seats {
		s := &seats[i]
		if _, ok := m.Seats[s.Spec.SeatKey]; ok || s.DeletionTimestamp != nil {
			continue
		}
		switch {
		case !s.Spec.Retired:
			patch := client.MergeFrom(s.DeepCopy())
			s.Spec.Retired = true
			if err := r.Client.Patch(ctx, s, patch); err != nil {
				return err
			}
			logf.FromContext(ctx).Info("retiring seat", "seat", s.Spec.SeatKey, "seatID", s.Spec.SeatID)
			r.event(org, corev1.EventTypeNormal, "SeatRetiring", "Retire", "seat %s removed from the declaration; winding down, then retiring with workspace retained", s.Spec.SeatKey)
		case s.Status.ExecutionState == v1alpha1.StateRetired:
			if err := r.Client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func seatSummaries(m *compile.Manifest, seats map[string]*v1alpha1.AgentSeat, nonce string) map[string]v1alpha1.SeatSummary {
	out := map[string]v1alpha1.SeatSummary{}
	for _, k := range sortedKeys(m.Seats) {
		sm := m.Seats[k]
		sum := v1alpha1.SeatSummary{ConfigRevision: sm.ConfigRevision, State: v1alpha1.StateProvisioning}
		if s := seats[k]; s != nil {
			sum.SeatID = s.Spec.SeatID
			if s.Status.ExecutionState != "" {
				sum.State = s.Status.ExecutionState
			}
			sum.AdoptedRevision = s.Status.AdoptedRevision
			if c := meta.FindStatusCondition(s.Status.Conditions, readiness.SeatReady); c != nil {
				sum.Ready = c.Status == metav1.ConditionTrue && s.Status.ObservedGeneration == s.Generation &&
					readiness.ProbeCurrent(s.Status.Probe, sm.ConfigRevision, nonce)
				if !sum.Ready {
					sum.Reason = truncate(c.Reason+": "+c.Message, 256)
				}
			}
		}
		out[k] = sum
	}
	return out
}

func (r *OrganizationReconciler) connectionDetails(org *v1alpha1.AgentOrganization, m *compile.Manifest, resp *runtimeapi.SyncResponse) *v1alpha1.ConnectionDetails {
	cd := &v1alpha1.ConnectionDetails{
		OrganizationID: resp.OrganizationID,
		Namespace:      org.Namespace,
		StatusEndpoint: strings.TrimRight(r.PlatformURL, "/") + runtimeapi.PathInternalOrgs + resp.OrganizationID + "/runtime",
	}
	for _, k := range sortedKeys(m.Spec.ChannelBindings) {
		b := m.Spec.ChannelBindings[k]
		if cd.Representatives == nil {
			cd.Representatives = map[string]v1alpha1.RepresentativeEndpoint{}
		}
		cd.Representatives[k] = v1alpha1.RepresentativeEndpoint{
			Seat: b.Seat, SeatID: resp.Seats[b.Seat].SeatID, Connection: b.Connection,
			Adapter: m.Spec.Connections[b.Connection].Adapter, ExternalUserID: b.ExternalUserID, Mode: b.Mode,
		}
	}
	return cd
}

// finalize implements organisation deletion per data_retention: capabilities
// are revoked through the platform, the runtime is removed and, with
// retention "retain", workspace volumes and durable rows are kept (A19).
func (r *OrganizationReconciler) finalize(ctx context.Context, org *v1alpha1.AgentOrganization) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(org, v1alpha1.FinalizerOrganization) {
		return ctrl.Result{}, nil
	}
	retention := org.Spec.DataRetention
	if retention != "delete" {
		retention = "retain"
	}
	orgKey := org.Spec.Key
	if id := org.Status.OrganizationID; id != "" {
		if err := r.Platform.DeleteOrganization(ctx, id, retention); err != nil {
			log.Error(err, "revoke organisation capabilities")
			r.event(org, corev1.EventTypeWarning, "RevokeFailed", "Delete", "platform could not revoke capabilities: %v", err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	seats, err := r.listSeats(ctx, org, orgKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range seats {
		if seats[i].DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, &seats[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	if len(seats) > 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if retention == "delete" {
		var pvcs corev1.PersistentVolumeClaimList
		if err := r.Client.List(ctx, &pvcs, client.InNamespace(org.Namespace), client.MatchingLabels{
			v1alpha1.LabelOrganization: orgKey, v1alpha1.LabelComponent: kube.ComponentSeat,
		}); err != nil {
			return ctrl.Result{}, err
		}
		for i := range pvcs.Items {
			if err := r.Client.Delete(ctx, &pvcs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	r.Pollers.Stop(org.Status.OrganizationID)
	r.mu.Lock()
	delete(r.syncs, org.UID)
	delete(r.verify, org.UID)
	r.mu.Unlock()
	patch := client.MergeFrom(org.DeepCopy())
	controllerutil.RemoveFinalizer(org, v1alpha1.FinalizerOrganization)
	log.Info("organisation removed", "dataRetention", retention)
	return ctrl.Result{}, r.Client.Patch(ctx, org, patch)
}
