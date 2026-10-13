package kube

import (
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/taskworker"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const LabelTaskRun = "steadmesh.io/task-run"
const LabelParentSeat = "steadmesh.io/parent-seat"

// TaskSeat gives the disposable runtime its own name and policy selector.
// Using the parent's seat label would union its broader network permissions.
func TaskSeat(parent *seatruntime.Seat, t runtimeapi.TaskRun) *seatruntime.Seat {
	s := *parent
	s.Name = t.ServiceAccount
	s.SeatKey = t.ServiceAccount
	s.Manifest = taskworker.Manifest(parent.Manifest, t.Request.ToolNames)
	s.AdoptFrom = ""
	return &s
}

func RenderTaskWorker(parent *seatruntime.Seat, t runtimeapi.TaskRun, o Options) (*corev1.Pod, *corev1.ServiceAccount, *networkingv1.NetworkPolicy, error) {
	s := TaskSeat(parent, t)
	sts, err := RenderStatefulSet(s, o, 1)
	if err != nil {
		return nil, nil, nil, err
	}
	pod := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta(s, s.Name, true), Spec: sts.Spec.Template.Spec}
	pod.Labels[LabelWorkload] = "task-worker"
	pod.Labels[LabelTaskRun] = t.ID
	pod.Labels[LabelParentSeat] = parent.SeatKey
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Spec.ActiveDeadlineSeconds = ptr.To(int64(t.Request.TimeoutSeconds))
	pod.Spec.TerminationGracePeriodSeconds = ptr.To(int64(10))
	c := &pod.Spec.Containers[0]
	c.Command = []string{RunnerBinary, "task-worker"}
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.Ports = nil
	c.Env = append(c.Env, corev1.EnvVar{Name: "STEADMESH_TASK_RUN_ID", Value: t.ID})
	// No persistent volume, shared workspace, ConfigMap, secret or parent token.
	c.VolumeMounts = []corev1.VolumeMount{{Name: "seat", MountPath: SeatMount}, {Name: "tmp", MountPath: "/tmp"}, {Name: "steadmesh-token", MountPath: TokenDir, ReadOnly: true}}
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "seat", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Gi"))}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Gi"))}}}, projectedToken(),
	}
	sa := RenderServiceAccount(s)
	sa.Labels[LabelTaskRun] = t.ID
	np := RenderNetworkPolicy(s, o)
	np.Labels[LabelTaskRun] = t.ID
	return pod, sa, np, nil
}
