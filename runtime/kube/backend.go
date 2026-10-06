package kube

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

// AnnotationProvides on a RuntimeClass declares the enforcement features it
// provides (comma-separated, e.g. "gvisor" or "microvm"), in addition to
// those inferred from well-known handlers.
const AnnotationProvides = "steadmesh.io/provides"

// BackendName is the execution profile backend this implementation serves.
const BackendName = "kubernetes"

// baseEnforcement is provided by every seat Pod of this backend.
var baseEnforcement = []string{"network_policy", "non_root", "read_only_root", "seccomp", "resource_limits", "runtime_class"}

// Backend implements runtime.Backend on Kubernetes.
type Backend struct {
	c    client.Client
	live client.Reader
	o    Options
	now  func() time.Time

	mu     sync.Mutex
	probes map[string]seatruntime.ProbeResult

	// ProbeFailureTTL is how long a failed enforcement probe is cached.
	ProbeFailureTTL time.Duration
	// ProbeTimeout bounds how long a probe Pod may take to complete.
	ProbeTimeout time.Duration
}

var _ seatruntime.Backend = (*Backend)(nil)

// New returns a Kubernetes backend. c may be cached; live must read the API
// server directly and is used for safety-relevant checks (Pod existence).
func New(c client.Client, live client.Reader, o Options) *Backend {
	return &Backend{
		c: c, live: live, o: o.withDefaults(), now: time.Now,
		probes:          map[string]seatruntime.ProbeResult{},
		ProbeFailureTTL: time.Minute,
		ProbeTimeout:    3 * time.Minute,
	}
}

// Options returns the effective options.
func (b *Backend) Options() Options { return b.o }

func (b *Backend) Capabilities(context.Context) seatruntime.Capabilities {
	return seatruntime.Capabilities{
		Backend:     BackendName,
		Features:    slices.Clone(compile.DefaultCatalog().Backends[BackendName].Features),
		Enforcement: append(slices.Clone(baseEnforcement), "gvisor", "microvm"),
	}
}

// runtimeClassProvides infers sandbox features from a RuntimeClass.
func runtimeClassProvides(rc *nodev1.RuntimeClass) []string {
	var out []string
	h := strings.ToLower(rc.Handler)
	switch {
	case strings.Contains(h, "runsc") || strings.Contains(h, "gvisor"):
		out = append(out, "gvisor")
	case strings.Contains(h, "kata") || strings.Contains(h, "firecracker") || h == "fc":
		out = append(out, "microvm")
	}
	for _, f := range strings.Split(rc.Annotations[AnnotationProvides], ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (b *Backend) Validate(ctx context.Context, s *seatruntime.Seat) error {
	m := s.Manifest
	if m.Execution.Backend != BackendName {
		return seatruntime.Errorf(seatruntime.Unsupported, "backend", "backend %q is not served by the kubernetes backend", m.Execution.Backend)
	}
	caps := b.Capabilities(ctx)
	switch m.Execution.IdlePolicy {
	case "", "warm", "warm_then_stop":
	case "suspend":
		return seatruntime.Errorf(seatruntime.Unsupported, "process_suspend", "idle_policy suspend requires process suspension, which this backend does not provide; refusing to substitute a weaker policy")
	default:
		return seatruntime.Errorf(seatruntime.Unsupported, "idle_policy", "unknown idle policy %q", m.Execution.IdlePolicy)
	}
	for _, f := range m.Execution.RequiredFeatures {
		if !slices.Contains(caps.Features, f) {
			return seatruntime.Errorf(seatruntime.Unsupported, f, "execution feature is not supported by the kubernetes backend")
		}
	}
	if ref := m.Sandbox.NetworkPolicyRef; ref != "" && ref != "deny_all_except_platform" {
		return seatruntime.Errorf(seatruntime.Unsupported, "network_policy", "network policy %q is not supported", ref)
	}
	if ref := m.Sandbox.FilesystemPolicyRef; ref != "" && ref != "workspace_only" {
		return seatruntime.Errorf(seatruntime.Unsupported, "filesystem_policy", "filesystem policy %q is not supported", ref)
	}
	if strings.TrimSpace(m.Harness.ImageDigest) == "" {
		return seatruntime.Errorf(seatruntime.Misconfigured, "harness_image", "harness profile %q has no image", m.HarnessProfile)
	}
	if _, err := resources(s); err != nil {
		return err
	}
	var provided []string
	if rcName := m.Sandbox.RuntimeClass; rcName != "" {
		var rc nodev1.RuntimeClass
		if err := b.live.Get(ctx, types.NamespacedName{Name: rcName}, &rc); err != nil {
			if apierrors.IsNotFound(err) {
				return seatruntime.Errorf(seatruntime.Misconfigured, "runtime_class", "RuntimeClass %q does not exist in the cluster; refusing to run with the default runtime", rcName)
			}
			return seatruntime.Errorf(seatruntime.Unavailable, "runtime_class", "cannot read RuntimeClass %q: %v", rcName, err)
		}
		provided = runtimeClassProvides(&rc)
	}
	for _, f := range m.Sandbox.RequiredEnforcement {
		switch {
		case f == "runtime_class":
			if m.Sandbox.RuntimeClass == "" {
				return seatruntime.Errorf(seatruntime.Misconfigured, f, "required enforcement runtime_class needs sandbox runtime_class to be set")
			}
		case slices.Contains(baseEnforcement, f):
		case f == "gvisor" || f == "microvm":
			if m.Sandbox.RuntimeClass == "" {
				return seatruntime.Errorf(seatruntime.Unsupported, f, "requires a RuntimeClass providing it; a basic container is not equivalent")
			}
			if !slices.Contains(provided, f) {
				return seatruntime.Errorf(seatruntime.Misconfigured, f, "RuntimeClass %q does not provide %s (annotate it with %s=%s if it does)", m.Sandbox.RuntimeClass, f, AnnotationProvides, f)
			}
		default:
			return seatruntime.Errorf(seatruntime.Unsupported, f, "enforcement feature is not supported by the kubernetes backend")
		}
	}
	return nil
}

// toApply converts a typed object into a server-side apply configuration,
// dropping status and null values so only declared fields are owned.
func toApply(obj client.Object) (*unstructured.Unstructured, error) {
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	delete(m, "status")
	pruneNil(m)
	return &unstructured.Unstructured{Object: m}, nil
}

func pruneNil(m map[string]any) {
	for k, v := range m {
		switch t := v.(type) {
		case nil:
			delete(m, k)
		case map[string]any:
			pruneNil(t)
		case []any:
			for _, e := range t {
				if em, ok := e.(map[string]any); ok {
					pruneNil(em)
				}
			}
		}
	}
}

func (b *Backend) apply(ctx context.Context, obj client.Object) error {
	u, err := toApply(obj)
	if err != nil {
		return err
	}
	return b.c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(b.o.FieldOwner), client.ForceOwnership)
}

func (b *Backend) PublishManifest(ctx context.Context, s *seatruntime.Seat, files map[string]string) error {
	return b.apply(ctx, RenderManifestConfigMap(s, files))
}

// ensureClaim creates the workspace PVC if it does not exist. An existing
// claim retained from a different (retired) seat identity is only reused when
// the seat explicitly adopts that identity (A18).
func (b *Backend) ensureClaim(ctx context.Context, s *seatruntime.Seat) error {
	want := RenderWorkspaceClaim(s)
	var cur corev1.PersistentVolumeClaim
	err := b.live.Get(ctx, client.ObjectKeyFromObject(want), &cur)
	if apierrors.IsNotFound(err) {
		if err := b.c.Create(ctx, want, client.FieldOwner(b.o.FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
			return seatruntime.Errorf(seatruntime.Unavailable, "persistent_workspace", "create workspace claim: %v", err)
		}
		return nil
	}
	if err != nil {
		return seatruntime.Errorf(seatruntime.Unavailable, "persistent_workspace", "read workspace claim: %v", err)
	}
	holder := cur.Annotations[v1alpha1.AnnotationSeatID]
	if holder == "" || holder == s.SeatID {
		return nil
	}
	if s.AdoptFrom == "" || s.AdoptFrom != holder {
		return seatruntime.Errorf(seatruntime.Misconfigured, "persistent_workspace",
			"claim %s holds the retained workspace of seat %s; set adopt_from to adopt it explicitly", cur.Name, holder)
	}
	patch := client.MergeFrom(cur.DeepCopy())
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[v1alpha1.AnnotationSeatID] = s.SeatID
	cur.Annotations["steadmesh.io/adopted-from"] = holder
	return b.c.Patch(ctx, &cur, patch, client.FieldOwner(b.o.FieldOwner))
}

func (b *Backend) Ensure(ctx context.Context, s *seatruntime.Seat, replicas int32) (*seatruntime.Status, error) {
	if err := b.Validate(ctx, s); err != nil {
		if serr := b.Stop(ctx, s); serr != nil {
			return nil, serr
		}
		st, _ := b.Status(ctx, s)
		return st, err
	}
	if err := b.apply(ctx, RenderServiceAccount(s)); err != nil {
		return nil, fmt.Errorf("apply service account: %w", err)
	}
	if err := b.ensureClaim(ctx, s); err != nil {
		if serr := b.Stop(ctx, s); serr != nil {
			return nil, serr
		}
		st, _ := b.Status(ctx, s)
		return st, err
	}
	// The policy exists before any workload that it restricts.
	if err := b.apply(ctx, RenderNetworkPolicy(s, b.o)); err != nil {
		return nil, fmt.Errorf("apply network policy: %w", err)
	}
	sts, err := RenderStatefulSet(s, b.o, replicas)
	if err != nil {
		return nil, err
	}
	if err := b.apply(ctx, sts); err != nil {
		return nil, fmt.Errorf("apply statefulset: %w", err)
	}
	return b.Status(ctx, s)
}

func podStatus(p *corev1.Pod) seatruntime.PodStatus {
	ps := seatruntime.PodStatus{
		Name: p.Name, UID: string(p.UID), Revision: p.Annotations[AnnotationConfigRevision],
		Phase: string(p.Status.Phase), Terminating: p.DeletionTimestamp != nil, NodeName: p.Spec.NodeName,
	}
	if p.Status.StartTime != nil {
		t := p.Status.StartTime.Time
		ps.StartTime = &t
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ps.Ready = true
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			ps.Waiting = cs.State.Waiting.Reason
		}
	}
	return ps
}

func (b *Backend) Status(ctx context.Context, s *seatruntime.Seat) (*seatruntime.Status, error) {
	st := &seatruntime.Status{BackendRef: "statefulset/" + s.Name, StorageHandle: names.WorkspaceClaim(s.Name)}
	var sts appsv1.StatefulSet
	switch err := b.c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &sts); {
	case err == nil:
		st.Exists = true
		if sts.Spec.Replicas != nil {
			st.Replicas = *sts.Spec.Replicas
		}
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	var pods corev1.PodList
	if err := b.c.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels(RunnerSelector(s))); err != nil {
		return nil, err
	}
	slices.SortFunc(pods.Items, func(a, c corev1.Pod) int { return strings.Compare(a.Name, c.Name) })
	for i := range pods.Items {
		st.Pods = append(st.Pods, podStatus(&pods.Items[i]))
	}
	var pvc corev1.PersistentVolumeClaim
	switch err := b.c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: st.StorageHandle}, &pvc); {
	case err == nil:
		st.StorageExists = true
		st.StoragePhase = string(pvc.Status.Phase)
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	var cm corev1.ConfigMap
	switch err := b.c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.ManifestConfigMap(s.Name)}, &cm); {
	case err == nil:
		st.ManifestRevision = cm.Annotations[AnnotationConfigRevision]
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	return st, nil
}

func (b *Backend) Quiesce(ctx context.Context, s *seatruntime.Seat, pod string) error {
	p := &corev1.Pod{}
	p.Name, p.Namespace = pod, s.Namespace
	// Graceful deletion only: the runner quiesces on SIGTERM within the grace period.
	if err := b.c.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (b *Backend) Stop(ctx context.Context, s *seatruntime.Seat) error {
	var sts appsv1.StatefulSet
	if err := b.c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &sts); err != nil {
		return client.IgnoreNotFound(err)
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
		return nil
	}
	patch := []byte(`{"spec":{"replicas":0}}`)
	return b.c.Patch(ctx, &sts, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(b.o.FieldOwner))
}

func (b *Backend) PodGone(ctx context.Context, s *seatruntime.Seat, uid string) (bool, error) {
	var pods corev1.PodList
	if err := b.live.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels(SeatLabels(s))); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if string(p.UID) == uid {
			return false, nil
		}
	}
	return true, nil
}

func (b *Backend) Cleanup(ctx context.Context, s *seatruntime.Seat, retention seatruntime.Retention) (bool, error) {
	if err := b.Stop(ctx, s); err != nil {
		return false, err
	}
	var pods corev1.PodList
	if err := b.live.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels(RunnerSelector(s))); err != nil {
		return false, err
	}
	if len(pods.Items) > 0 {
		// Let the runner quiesce and checkpoint before removing its workload.
		return false, nil
	}
	objs := []client.Object{
		&appsv1.StatefulSet{}, &networkingv1.NetworkPolicy{}, &corev1.ServiceAccount{}, &corev1.ConfigMap{},
	}
	objNames := []string{s.Name, s.Name, s.Name, names.ManifestConfigMap(s.Name)}
	if retention == seatruntime.DeleteData {
		objs = append(objs, &corev1.PersistentVolumeClaim{})
		objNames = append(objNames, names.WorkspaceClaim(s.Name))
	}
	for i, o := range objs {
		o.SetNamespace(s.Namespace)
		o.SetName(objNames[i])
		if err := b.c.Delete(ctx, o, client.PropagationPolicy("Background")); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}
