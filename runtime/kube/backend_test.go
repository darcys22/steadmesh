package kube

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/names"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

// newFake returns a fake client. The fake's server-side apply type converter
// rejects some built-in types (NetworkPolicy), so apply is emulated as
// create-or-replace here; real apply semantics are covered by envtest.
func newFake(objs ...client.Object) client.Client {
	sch := k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = v1alpha1.AddToScheme(sch)
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, ac k8sruntime.ApplyConfiguration, _ ...client.ApplyOption) error {
			u, ok := ac.(interface{ UnstructuredContent() map[string]any })
			if !ok {
				return fmt.Errorf("unsupported apply configuration %T", ac)
			}
			uo := &unstructured.Unstructured{Object: u.UnstructuredContent()}
			obj, err := sch.New(uo.GroupVersionKind())
			if err != nil {
				return err
			}
			if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(uo.Object, obj); err != nil {
				return err
			}
			co := obj.(client.Object)
			cur := co.DeepCopyObject().(client.Object)
			if err := c.Get(ctx, client.ObjectKeyFromObject(co), cur); apierrors.IsNotFound(err) {
				return c.Create(ctx, co)
			} else if err != nil {
				return err
			}
			co.SetResourceVersion(cur.GetResourceVersion())
			return c.Update(ctx, co)
		}}).Build()
}

func plainSeat() *seatruntime.Seat {
	s := testSeat()
	s.Manifest.Sandbox.RuntimeClass = ""
	return s
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	gv := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}
	c := newFake(gv)
	b := New(c, c, testOptions())

	cases := []struct {
		name string
		mut  func(s *seatruntime.Seat)
		kind seatruntime.ErrorKind
	}{
		{"plain", func(s *seatruntime.Seat) {}, ""},
		{"existing runtime class", func(s *seatruntime.Seat) { s.Manifest.Sandbox.RuntimeClass = "gvisor" }, ""},
		{"missing runtime class", func(s *seatruntime.Seat) { s.Manifest.Sandbox.RuntimeClass = "kata" }, seatruntime.Misconfigured},
		{"gvisor without runtime class", func(s *seatruntime.Seat) { s.Manifest.Sandbox.RequiredEnforcement = []string{"gvisor"} }, seatruntime.Unsupported},
		{"gvisor provided", func(s *seatruntime.Seat) {
			s.Manifest.Sandbox.RuntimeClass = "gvisor"
			s.Manifest.Sandbox.RequiredEnforcement = []string{"gvisor"}
		}, ""},
		{"microvm not provided by gvisor", func(s *seatruntime.Seat) {
			s.Manifest.Sandbox.RuntimeClass = "gvisor"
			s.Manifest.Sandbox.RequiredEnforcement = []string{"microvm"}
		}, seatruntime.Misconfigured},
		{"unknown enforcement", func(s *seatruntime.Seat) { s.Manifest.Sandbox.RequiredEnforcement = []string{"sibling_isolation"} }, seatruntime.Unsupported},
		{"suspend", func(s *seatruntime.Seat) { s.Manifest.Execution.IdlePolicy = "suspend" }, seatruntime.Unsupported},
		{"other backend", func(s *seatruntime.Seat) { s.Manifest.Execution.Backend = "agentcontainers" }, seatruntime.Unsupported},
		{"unsupported feature", func(s *seatruntime.Seat) { s.Manifest.Execution.RequiredFeatures = []string{"process_snapshot"} }, seatruntime.Unsupported},
		{"no image", func(s *seatruntime.Seat) { s.Manifest.Harness.ImageDigest = "" }, seatruntime.Misconfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := plainSeat()
			tc.mut(s)
			err := b.Validate(ctx, s)
			if got := seatruntime.KindOf(err); got != tc.kind || (tc.kind == "" && err != nil) {
				t.Fatalf("kind = %q (%v), want %q", got, err, tc.kind)
			}
		})
	}
}

func TestEnsureCreatesObjectsAndBlocksWithoutFallback(t *testing.T) {
	ctx := context.Background()
	c := newFake()
	b := New(c, c, testOptions())
	s := plainSeat()
	st, err := b.Ensure(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Replicas != 0 || !st.StorageExists {
		t.Fatalf("status = %+v", st)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: names.WorkspaceClaim(s.Name)}, &pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("PVC must not be owned")
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, &np); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Ensure(ctx, s, 1); err != nil {
		t.Fatal(err)
	}
	// Now request an absent RuntimeClass: the workload is scaled to zero, not
	// restarted without the sandbox.
	s.Manifest.Sandbox.RuntimeClass = "kata"
	_, err = b.Ensure(ctx, s, 1)
	if seatruntime.KindOf(err) != seatruntime.Misconfigured {
		t.Fatalf("err = %v", err)
	}
	var sts appsv1.StatefulSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, &sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 0 || sts.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatal("blocked seat must be scaled to zero and keep its previous template")
	}

	// A fresh blocked seat gets no StatefulSet at all.
	s2 := plainSeat()
	s2.Name, s2.SeatKey = names.Seat("acme", "other"), "other"
	s2.Manifest.Sandbox.RuntimeClass = "kata"
	if _, err := b.Ensure(ctx, s2, 1); seatruntime.KindOf(err) != seatruntime.Misconfigured {
		t.Fatalf("err = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: s2.Namespace, Name: s2.Name}, &sts); !apierrors.IsNotFound(err) {
		t.Fatalf("statefulset must not exist: %v", err)
	}
}

func TestRetainedWorkspaceRequiresAdoption(t *testing.T) {
	ctx := context.Background()
	c := newFake()
	b := New(c, c, testOptions())
	old := plainSeat()
	if _, err := b.Ensure(ctx, old, 0); err != nil {
		t.Fatal(err)
	}
	if done, err := b.Cleanup(ctx, old, seatruntime.RetainData); err != nil || !done {
		t.Fatalf("cleanup = %v %v", done, err)
	}
	recreated := plainSeat()
	recreated.SeatID = "seat-2"
	if _, err := b.Ensure(ctx, recreated, 0); seatruntime.KindOf(err) != seatruntime.Misconfigured {
		t.Fatalf("recreated seat must not silently receive the retained workspace: %v", err)
	}
	recreated.AdoptFrom = "seat-1"
	if _, err := b.Ensure(ctx, recreated, 0); err != nil {
		t.Fatalf("explicit adoption: %v", err)
	}
	var pvc corev1.PersistentVolumeClaim
	_ = c.Get(ctx, client.ObjectKey{Namespace: old.Namespace, Name: names.WorkspaceClaim(old.Name)}, &pvc)
	if pvc.Annotations[v1alpha1.AnnotationSeatID] != "seat-2" {
		t.Fatal("adoption must rebind the claim")
	}
}

func TestCleanupDeleteRemovesClaim(t *testing.T) {
	ctx := context.Background()
	c := newFake()
	b := New(c, c, testOptions())
	s := plainSeat()
	if _, err := b.Ensure(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	if done, err := b.Cleanup(ctx, s, seatruntime.DeleteData); err != nil || !done {
		t.Fatal(done, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: names.WorkspaceClaim(s.Name)}, &pvc); !apierrors.IsNotFound(err) {
		t.Fatal("claim must be deleted with retention delete")
	}
}

func TestCleanupWaitsForPods(t *testing.T) {
	ctx := context.Background()
	s := plainSeat()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(s.Name), Namespace: s.Namespace, Labels: RunnerSelector(s)}}
	c := newFake(pod)
	b := New(c, c, testOptions())
	if _, err := b.Ensure(ctx, s, 1); err != nil {
		t.Fatal(err)
	}
	done, err := b.Cleanup(ctx, s, seatruntime.RetainData)
	if err != nil || done {
		t.Fatalf("cleanup must wait for the runner pod: %v %v", done, err)
	}
	var sts appsv1.StatefulSet
	_ = c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, &sts)
	if *sts.Spec.Replicas != 0 {
		t.Fatal("cleanup must scale to zero first")
	}
}

func TestEnforcementProbe(t *testing.T) {
	ctx := context.Background()
	c := newFake()
	b := New(c, c, testOptions())
	now := time.Now()
	b.now = func() time.Time { return now }
	s := plainSeat()
	req := seatruntime.EnforcementProbe{Namespace: s.Namespace, ProfileDigest: ProfileDigest(s.Manifest.Sandbox), Seat: s}

	r, err := b.VerifyEnforcement(ctx, req)
	if err != nil || r.State != seatruntime.ProbePending {
		t.Fatalf("without the seat policy the probe must wait: %+v %v", r, err)
	}
	var pods corev1.PodList
	_ = c.List(ctx, &pods)
	if len(pods.Items) != 0 {
		t.Fatal("no probe pod before the policy exists")
	}
	if _, err := b.Ensure(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	if r, _ = b.VerifyEnforcement(ctx, req); r.State != seatruntime.ProbePending {
		t.Fatalf("state = %s", r.State)
	}
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: s.Namespace, Name: ProbePodName(req.ProfileDigest)}
	if err := c.Get(ctx, key, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netprobe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "denied url reachable"}}}}
	if err := c.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	r, _ = b.VerifyEnforcement(ctx, req)
	if r.State != seatruntime.ProbeFailed || r.Detail == "" {
		t.Fatalf("result = %+v", r)
	}
	// Cached while within the failure TTL, even though the pod is gone.
	if r2, _ := b.VerifyEnforcement(ctx, req); r2.State != seatruntime.ProbeFailed {
		t.Fatal("failed result must be cached")
	}
	now = now.Add(2 * time.Minute)
	if r, _ = b.VerifyEnforcement(ctx, req); r.State != seatruntime.ProbePending {
		t.Fatalf("after the TTL the probe reruns: %+v", r)
	}
	_ = c.Get(ctx, key, &pod)
	pod.Status.Phase = corev1.PodSucceeded
	_ = c.Status().Update(ctx, &pod)
	if r, _ = b.VerifyEnforcement(ctx, req); r.State != seatruntime.ProbePassed {
		t.Fatalf("result = %+v", r)
	}
	if cached, ok := b.CachedEnforcement(s.Namespace, req.ProfileDigest); !ok || cached.State != seatruntime.ProbePassed {
		t.Fatal("passed result must be cached")
	}
	// A new nonce forces a fresh probe.
	req.Nonce = "verify-2"
	if r, _ = b.VerifyEnforcement(ctx, req); r.State != seatruntime.ProbePending {
		t.Fatalf("a new nonce must re-probe: %+v", r)
	}
}
