package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	"github.com/darcys22/steadmesh/runtime/kube"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type taskPlatform struct {
	platform.API
	mu      sync.Mutex
	run     runtimeapi.TaskRun
	actions []string
}

func (p *taskPlatform) TaskRuns(context.Context, string) ([]runtimeapi.TaskRun, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.run.CleanedAt != nil {
		return nil, nil
	}
	return []runtimeapi.TaskRun{p.run}, nil
}
func (p *taskPlatform) TaskRunAction(_ context.Context, _, _, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, action)
	switch action {
	case "launch":
		if p.run.Terminal() || p.run.LaunchStartedAt != nil || p.run.CleanedAt != nil {
			return errors.New("launch fenced")
		}
		now := time.Now()
		p.run.LaunchStartedAt = &now
	case "launched":
		if p.run.LaunchStartedAt == nil {
			return errors.New("launch not started")
		}
		now := time.Now()
		p.run.LaunchResolvedAt = &now
	case "cleaned":
		if !p.run.Terminal() || (p.run.LaunchStartedAt != nil && p.run.LaunchResolvedAt == nil) {
			return errors.New("cleanup fenced")
		}
		now := time.Now()
		p.run.CleanedAt = &now
	case "cancel":
		p.run.State = runtimeapi.TaskCancelled
	case "failed":
		p.run.State = runtimeapi.TaskFailed
	}
	return nil
}

func TestTaskWorkersReconcileAndCleanup(t *testing.T) {
	scheme := kruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	api := &taskPlatform{run: runtimeapi.TaskRun{ID: "id", ServiceAccount: "task-id", ParentSeatID: "parent", State: runtimeapi.TaskPending, Deadline: time.Now().Add(time.Minute), Request: runtimeapi.TaskRunRequest{TimeoutSeconds: 60}}}
	backend := kube.New(c, c, kube.Options{})
	r := &SeatReconciler{Client: c, Live: c, Backend: backend, Platform: api}
	parent := &seatruntime.Seat{Namespace: "acme", Name: "parent", SeatID: "parent", SeatKey: "engineer", OrgKey: "acme", Manifest: compile.SeatManifest{Harness: spec.HarnessProfile{ImageDigest: "fake:dev"}, Execution: spec.ExecutionProfile{CPULimit: "1", MemoryLimit: "1Gi"}}, Owner: &metav1.OwnerReference{UID: "owner", Name: "seat", Kind: "AgentSeat", APIVersion: "steadmesh.io/v1alpha1"}}
	ctx := context.Background()
	if err := r.reconcileTaskWorkers(ctx, parent, true); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "acme", Name: "task-id"}
	var pod corev1.Pod
	if err := c.Get(ctx, key, &pod); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if api.run.State != runtimeapi.TaskCancelled {
		t.Fatal("ineligible parent did not revoke child")
	}
	if err := r.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if api.run.CleanedAt == nil {
		t.Fatal("cleanup not recorded")
	}
	for _, obj := range []client.Object{&corev1.Pod{}, &corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
		if err := c.Get(ctx, key, obj); err == nil {
			t.Fatalf("left runtime object %T", obj)
		}
	}
}
func TestTaskWorkerClaimedPodNeverRestarts(t *testing.T) {
	scheme := kruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	api := &taskPlatform{run: runtimeapi.TaskRun{ID: "id", ServiceAccount: "task-id", State: runtimeapi.TaskRunning, PodUID: "vanished", Deadline: time.Now().Add(time.Minute)}}
	r := &SeatReconciler{Client: c, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	if err := r.reconcileTaskWorkers(context.Background(), &seatruntime.Seat{Namespace: "acme", SeatID: "parent"}, true); err != nil {
		t.Fatal(err)
	}
	if api.run.State != runtimeapi.TaskFailed {
		t.Fatal("claimed worker loss not terminal")
	}
	var pods corev1.PodList
	_ = c.List(context.Background(), &pods)
	if len(pods.Items) != 0 {
		t.Fatal("claimed worker restarted")
	}
}

func TestTaskWorkerWaitsForMatchingParentManifest(t *testing.T) {
	scheme := kruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	api := &taskPlatform{run: runtimeapi.TaskRun{ID: "id", ServiceAccount: "task-id", State: runtimeapi.TaskPending, ConfigRevision: "new-policy", Deadline: time.Now().Add(time.Minute)}}
	r := &SeatReconciler{Client: c, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	if err := r.reconcileTaskWorkers(context.Background(), &seatruntime.Seat{Namespace: "acme", SeatID: "parent", ConfigRevision: "old-policy"}, true); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	_ = c.List(context.Background(), &pods)
	if len(pods.Items) != 0 || len(api.actions) != 0 {
		t.Fatal("pending worker launched or failed under stale parent revision")
	}
}

// A blocked Create models an API request that reaches Kubernetes after another
// controller has reconciled cancellation. The reservation must cover that gap.
type delayedTaskClient struct {
	client.Client
	started   chan struct{}
	release   chan struct{}
	createErr error
}

func (c *delayedTaskClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		close(c.started)
		<-c.release
		if c.createErr != nil {
			return c.createErr
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

func taskLaunchFixture(t *testing.T) (client.Client, *taskPlatform, *seatruntime.Seat) {
	t.Helper()
	scheme := kruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	api := &taskPlatform{run: runtimeapi.TaskRun{ID: "id", ServiceAccount: "task-id", ParentSeatID: "parent", State: runtimeapi.TaskPending, Deadline: time.Now().Add(time.Minute), Request: runtimeapi.TaskRunRequest{TimeoutSeconds: 60}}}
	parent := &seatruntime.Seat{Namespace: "acme", Name: "parent", SeatID: "parent", SeatKey: "engineer", OrgKey: "acme", Manifest: compile.SeatManifest{Harness: spec.HarnessProfile{ImageDigest: "fake:dev"}, Execution: spec.ExecutionProfile{CPULimit: "1", MemoryLimit: "1Gi"}}}
	return c, api, parent
}

func TestTaskWorkerDelayedCreateCannotReleaseCapacity(t *testing.T) {
	c, api, parent := taskLaunchFixture(t)
	blocked := &delayedTaskClient{Client: c, started: make(chan struct{}), release: make(chan struct{})}
	creator := &SeatReconciler{Client: blocked, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	cleaner := &SeatReconciler{Client: c, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- creator.reconcileTaskWorkers(ctx, parent, true) }()
	<-blocked.started
	if err := cleaner.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if err := cleaner.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
		if err := c.Get(ctx, types.NamespacedName{Namespace: parent.Namespace, Name: "task-id"}, obj); err != nil {
			t.Fatalf("removed runtime protection during unresolved launch: %T: %v", obj, err)
		}
	}
	runs, _ := api.TaskRuns(ctx, parent.SeatID)
	if len(runs) != 1 || runs[0].CleanedAt != nil || runs[0].LaunchStartedAt == nil || runs[0].LaunchResolvedAt != nil {
		t.Fatal("capacity released while Pod Create remains in flight")
	}
	close(blocked.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Successful return resolves the launch, but cleanup must still delete the Pod.
	if err := cleaner.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if api.run.CleanedAt != nil {
		t.Fatal("cleanup acknowledged before confirmed absence")
	}
	if err := cleaner.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if api.run.CleanedAt == nil {
		t.Fatal("conclusive launch and deletion did not release capacity")
	}
}

func TestTaskWorkerAmbiguousCreateSurvivesRestartFailClosed(t *testing.T) {
	c, api, parent := taskLaunchFixture(t)
	released := make(chan struct{})
	close(released)
	broken := &delayedTaskClient{Client: c, started: make(chan struct{}), release: released, createErr: context.DeadlineExceeded}
	r := &SeatReconciler{Client: broken, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	ctx := context.Background()
	if err := r.reconcileTaskWorkers(ctx, parent, true); err == nil {
		t.Fatal("expected ambiguous create error")
	}
	// A new process has no launch-owner knowledge and must not retry or resolve.
	restarted := &SeatReconciler{Client: c, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	if err := restarted.reconcileTaskWorkers(ctx, parent, true); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatal("ambiguous launch retried after restart")
	}
	if err := restarted.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileTaskWorkers(ctx, parent, false); err != nil {
		t.Fatal(err)
	}
	if api.run.CleanedAt != nil || api.run.LaunchResolvedAt != nil {
		t.Fatal("ambiguous launch reservation reclaimed")
	}
}

func TestTaskWorkerResolvedUnclaimedPodNeverRestarts(t *testing.T) {
	c, api, parent := taskLaunchFixture(t)
	now := time.Now()
	api.run.LaunchStartedAt = &now
	api.run.LaunchResolvedAt = &now
	r := &SeatReconciler{Client: c, Live: c, Backend: kube.New(c, c, kube.Options{}), Platform: api}
	if err := r.reconcileTaskWorkers(context.Background(), parent, true); err != nil {
		t.Fatal(err)
	}
	if api.run.State != runtimeapi.TaskFailed {
		t.Fatal("lost unclaimed Pod not failed")
	}
}
