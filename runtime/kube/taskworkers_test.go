package kube

import (
	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestTaskWorkerIsDisposableAndRestricted(t *testing.T) {
	parent := testSeat()
	task := runtimeapi.TaskRun{ID: "run", ServiceAccount: "task-run", Request: runtimeapi.TaskRunRequest{TimeoutSeconds: 60}}
	pod, sa, np, err := RenderTaskWorker(parent, task, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.ServiceAccountName == parent.Name || sa.Name == parent.Name || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("inherited parent token")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.ActiveDeadlineSeconds != 60 {
		t.Fatal("unbounded/restarting child")
	}
	if pod.Labels[v1alpha1.LabelSeat] == parent.SeatKey || np.Spec.PodSelector.MatchLabels[v1alpha1.LabelSeat] != pod.Labels[v1alpha1.LabelSeat] {
		t.Fatal("worker can inherit broader network policy")
	}
	if len(np.Spec.Egress) != 2 || len(np.Spec.Ingress) != 0 {
		t.Fatalf("unexpected network access: %+v", np.Spec)
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil || v.Secret != nil || v.ConfigMap != nil || v.HostPath != nil {
			t.Fatalf("inherited external data volume: %+v", v)
		}
	}
	if pod.Spec.Volumes[0].EmptyDir == nil || pod.Spec.Containers[0].Command[1] != "task-worker" || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatal("isolation/runtime contract")
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != parent.Owner.UID {
		t.Fatal("worker not owned by parent")
	}
	if len(pod.Spec.Containers[0].Resources.Limits) != 2 {
		t.Fatal("missing parent resource limits")
	}
}
