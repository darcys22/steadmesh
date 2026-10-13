package controller

import (
	"context"
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
	run     runtimeapi.TaskRun
	actions []string
}

func (p *taskPlatform) TaskRuns(context.Context, string) ([]runtimeapi.TaskRun, error) {
	if p.run.CleanedAt != nil {
		return nil, nil
	}
	return []runtimeapi.TaskRun{p.run}, nil
}
func (p *taskPlatform) TaskRunAction(_ context.Context, _, _, action string) error {
	p.actions = append(p.actions, action)
	switch action {
	case "cleaned":
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
