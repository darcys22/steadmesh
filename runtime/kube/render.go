// Package kube is the Kubernetes implementation of the sandbox backend
// (ADR-0002): per seat a ServiceAccount, a retained workspace PVC, a manifest
// ConfigMap, a default-deny NetworkPolicy and a StatefulSet with 0 or 1
// replicas. The rendering functions in this file are pure and implement the
// Seat Pod contract in docs/architecture.html#seat-pod.
package kube

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/names"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

const (
	// LabelWorkload distinguishes the seat runner Pod from other seat-labelled
	// Pods (the enforcement probe), so the StatefulSet never adopts them.
	LabelWorkload = "steadmesh.io/workload"
	// LabelProbe marks enforcement probe Pods.
	LabelProbe = "steadmesh.io/probe"
	// AnnotationConfigRevision records the config revision of a Pod template
	// or manifest ConfigMap.
	AnnotationConfigRevision = "steadmesh.io/config-revision"

	WorkloadRunner = "seat-runner"
	ComponentSeat  = "seat"
	// ComponentPlatform labels the platform Pods seats may reach.
	ComponentPlatform = "platform"

	ContainerName   = "seat-runner"
	RunnerBinary    = "/usr/local/bin/seat-runner"
	TokenDir        = "/var/run/steadmesh"
	TokenPath       = TokenDir + "/token"
	TokenExpiration = int64(3600)
	SeatMount       = "/seat"
	ManifestDir     = "/etc/steadmesh/manifest"
	RunnerUID       = int64(1000)
	HealthPort      = int32(8081)
	PlatformPort    = int32(8080)
	GracePeriod     = int64(300)

	ManifestFile     = "manifest.json"
	InstructionsFile = "instructions.md"
)

// Options configures the Kubernetes backend.
type Options struct {
	// PlatformURL is propagated to seats as STEADMESH_PLATFORM_URL.
	PlatformURL string
	// ControlPlaneNamespace runs the platform Pods seats may reach.
	ControlPlaneNamespace string
	// ImagePullPolicy of seat and probe containers.
	ImagePullPolicy corev1.PullPolicy
	// NetprobeImage runs `seat-runner netprobe`.
	NetprobeImage string
	// NetprobeDeniedURL must be unreachable from a seat when policy is enforced.
	NetprobeDeniedURL string
	// DNSNamespace hosts the cluster DNS service seats may query.
	DNSNamespace string
	// FieldOwner is the server-side apply field manager.
	FieldOwner string
	// EgressURL is the egress gateway, empty when it is not enabled. Seats
	// whose access needs it reach its Pods (component egress) only.
	EgressURL string
}

// ComponentEgress labels the egress gateway Pods.
const ComponentEgress = "egress"

// egressPort is the gateway's port from EgressURL.
func (o Options) egressPort() int32 {
	u, err := url.Parse(o.EgressURL)
	if err != nil {
		return 3128
	}
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return int32(p)
	}
	return 3128
}

func (o Options) withDefaults() Options {
	if o.ControlPlaneNamespace == "" {
		o.ControlPlaneNamespace = "steadmesh-system"
	}
	if o.PlatformURL == "" {
		o.PlatformURL = "http://steadmesh-platform." + o.ControlPlaneNamespace + ".svc:8080"
	}
	if o.ImagePullPolicy == "" {
		o.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if o.NetprobeImage == "" {
		o.NetprobeImage = "steadmesh/seat-fake:dev"
	}
	if o.NetprobeDeniedURL == "" {
		o.NetprobeDeniedURL = "https://kubernetes.default.svc:443/livez"
	}
	if o.DNSNamespace == "" {
		o.DNSNamespace = "kube-system"
	}
	if o.FieldOwner == "" {
		o.FieldOwner = "steadmesh-controller"
	}
	return o
}

// SeatLabels are the labels of every seat object.
func SeatLabels(s *seatruntime.Seat) map[string]string {
	return map[string]string{
		v1alpha1.LabelOrganization: s.OrgKey,
		v1alpha1.LabelSeat:         s.SeatKey,
		v1alpha1.LabelComponent:    ComponentSeat,
	}
}

// policySelector selects the Pods the seat NetworkPolicy applies to: every
// Pod carrying the seat's organisation and seat labels.
func policySelector(s *seatruntime.Seat) map[string]string {
	return map[string]string{
		v1alpha1.LabelOrganization: s.OrgKey,
		v1alpha1.LabelSeat:         s.SeatKey,
	}
}

// RunnerSelector selects the seat runner Pod of the StatefulSet.
func RunnerSelector(s *seatruntime.Seat) map[string]string {
	l := SeatLabels(s)
	l[LabelWorkload] = WorkloadRunner
	return l
}

func meta(s *seatruntime.Seat, name string, owned bool) metav1.ObjectMeta {
	m := metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: SeatLabels(s)}
	if owned && s.Owner != nil {
		m.OwnerReferences = []metav1.OwnerReference{*s.Owner}
	}
	return m
}

func identityAnnotations(s *seatruntime.Seat) map[string]string {
	return map[string]string{v1alpha1.AnnotationSeatID: s.SeatID, v1alpha1.AnnotationOrgID: s.OrgID}
}

// RenderServiceAccount renders the seat identity. The Kubernetes API token is
// never mounted; the seat only receives a gateway-audience projected token.
func RenderServiceAccount(s *seatruntime.Seat) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   meta(s, s.Name, true),
		AutomountServiceAccountToken: ptr.To(false),
	}
	sa.Annotations = identityAnnotations(s)
	return sa
}

// RenderWorkspaceClaim renders the workspace PVC. It deliberately has no owner
// reference so it survives retirement and organisation deletion (§5.5).
func RenderWorkspaceClaim(s *seatruntime.Seat) *corev1.PersistentVolumeClaim {
	size := s.Manifest.Execution.WorkspaceSizeGB
	if size <= 0 {
		size = 5
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: meta(s, names.WorkspaceClaim(s.Name), false),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(size<<30, resource.BinarySI)},
			},
		},
	}
	pvc.Annotations = identityAnnotations(s)
	if sc := s.Manifest.Execution.StorageClass; sc != "" {
		pvc.Spec.StorageClassName = ptr.To(sc)
	}
	return pvc
}

// RenderManifestConfigMap renders the read-only manifest ConfigMap.
func RenderManifestConfigMap(s *seatruntime.Seat, files map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: meta(s, names.ManifestConfigMap(s.Name), true),
		Data:       files,
	}
	cm.Annotations = map[string]string{AnnotationConfigRevision: s.ConfigRevision}
	return cm
}

// RenderNetworkPolicy renders the default-deny seat policy: no ingress; egress
// only to platform Pods in the control-plane namespace on TCP 8080 and DNS,
// plus what the seat's access allows: the egress gateway Pods when the seat
// uses it, and the network plugin's CIDR rules.
func RenderNetworkPolicy(s *seatruntime.Seat, o Options) *networkingv1.NetworkPolicy {
	o = o.withDefaults()
	np := renderBasePolicy(s, o)
	a := s.Manifest.Access
	tcp := corev1.ProtocolTCP
	if a.UsesGateway() && o.EgressURL != "" {
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": o.ControlPlaneNamespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelComponent: ComponentEgress}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(o.egressPort()))}},
		})
	}
	if a != nil {
		for _, r := range a.Network {
			proto := corev1.Protocol(strings.ToUpper(r.Protocol))
			rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}}}
			if len(r.Ports) == 0 {
				rule.Ports = []networkingv1.NetworkPolicyPort{{Protocol: &proto}}
			}
			for _, p := range r.Ports {
				rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: ptr.To(intstr.FromInt32(int32(p)))})
			}
			np.Spec.Egress = append(np.Spec.Egress, rule)
		}
	}
	return np
}

func renderBasePolicy(s *seatruntime.Seat, o Options) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: meta(s, s.Name, true),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: policySelector(s)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": o.ControlPlaneNamespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelComponent: ComponentPlatform}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(PlatformPort))}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": o.DNSNamespace}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: ptr.To(intstr.FromInt32(53))},
						{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(53))},
					},
				},
			},
		},
	}
}

func restrictedPodSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:        ptr.To(true),
		RunAsUser:           ptr.To(RunnerUID),
		RunAsGroup:          ptr.To(RunnerUID),
		FSGroup:             ptr.To(RunnerUID),
		FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func restrictedContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To(RunnerUID),
		RunAsGroup:               ptr.To(RunnerUID),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Privileged:               ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func resources(s *seatruntime.Seat) (corev1.ResourceRequirements, error) {
	e := s.Manifest.Execution
	r := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, q := range []struct {
		val  string
		name corev1.ResourceName
		dst  corev1.ResourceList
	}{
		{e.CPURequest, corev1.ResourceCPU, r.Requests}, {e.CPULimit, corev1.ResourceCPU, r.Limits},
		{e.MemoryRequest, corev1.ResourceMemory, r.Requests}, {e.MemoryLimit, corev1.ResourceMemory, r.Limits},
	} {
		if q.val == "" {
			continue
		}
		parsed, err := resource.ParseQuantity(q.val)
		if err != nil {
			return r, seatruntime.Errorf(seatruntime.Misconfigured, "resource_limits", "invalid quantity %q", q.val)
		}
		q.dst[q.name] = parsed
	}
	// Bounded resources are a required enforcement feature: never run without limits.
	if _, ok := r.Limits[corev1.ResourceCPU]; !ok {
		return r, seatruntime.Errorf(seatruntime.Misconfigured, "resource_limits", "cpu_limit is required")
	}
	if _, ok := r.Limits[corev1.ResourceMemory]; !ok {
		return r, seatruntime.Errorf(seatruntime.Misconfigured, "resource_limits", "memory_limit is required")
	}
	return r, nil
}

func seatEnv(s *seatruntime.Seat, o Options) []corev1.EnvVar {
	h := s.Manifest.Harness
	return []corev1.EnvVar{
		{Name: "STEADMESH_PLATFORM_URL", Value: o.PlatformURL},
		{Name: "STEADMESH_TOKEN_FILE", Value: TokenPath},
		{Name: "STEADMESH_ORG_ID", Value: s.OrgID},
		{Name: "STEADMESH_SEAT_ID", Value: s.SeatID},
		{Name: "STEADMESH_SEAT_KEY", Value: s.SeatKey},
		{Name: "STEADMESH_CONFIG_REVISION", Value: s.ConfigRevision},
		{Name: "STEADMESH_HARNESS", Value: h.Adapter},
		{Name: "STEADMESH_MANIFEST_DIR", Value: ManifestDir},
		{Name: "STEADMESH_EGRESS_URL", Value: egressURL(s, o)},
		{Name: "HOME", Value: SeatMount + "/home"},
		{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
	}
}

// egressURL is the gateway URL for a seat that uses it.
func egressURL(s *seatruntime.Seat, o Options) string {
	if s.Manifest.Access.UsesGateway() {
		return o.EgressURL
	}
	return ""
}

func projectedToken() corev1.Volume {
	return corev1.Volume{
		Name: "steadmesh-token",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To(int32(0o440)),
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience:          v1alpha1.GatewayAudience,
				ExpirationSeconds: ptr.To(TokenExpiration),
				Path:              "token",
			}}},
		}},
	}
}

// RenderStatefulSet renders the seat workload with the given replica count.
// The update strategy is OnDelete: the controller replaces the Pod at a safe
// boundary when the config revision changes (A13, A14).
func RenderStatefulSet(s *seatruntime.Seat, o Options, replicas int32) (*appsv1.StatefulSet, error) {
	o = o.withDefaults()
	if replicas < 0 || replicas > 1 {
		return nil, fmt.Errorf("replicas must be 0 or 1, got %d", replicas)
	}
	image := s.Manifest.Harness.ImageDigest
	if strings.TrimSpace(image) == "" {
		return nil, seatruntime.Errorf(seatruntime.Misconfigured, "harness_image", "harness profile %q has no image", s.Manifest.HarnessProfile)
	}
	res, err := resources(s)
	if err != nil {
		return nil, err
	}
	tmplLabels := RunnerSelector(s)
	pod := corev1.PodSpec{
		ServiceAccountName:            s.Name,
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		TerminationGracePeriodSeconds: ptr.To(GracePeriod),
		RestartPolicy:                 corev1.RestartPolicyAlways,
		SecurityContext:               restrictedPodSecurity(),
		Containers: []corev1.Container{{
			Name:            ContainerName,
			Image:           image,
			ImagePullPolicy: o.ImagePullPolicy,
			Command:         []string{RunnerBinary},
			Env:             seatEnv(s, o),
			Resources:       res,
			SecurityContext: restrictedContainerSecurity(),
			Ports:           []corev1.ContainerPort{{Name: "health", ContainerPort: HealthPort, Protocol: corev1.ProtocolTCP}},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(HealthPort)}},
				PeriodSeconds:    10,
				FailureThreshold: 6,
				TimeoutSeconds:   5,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(HealthPort)}},
				PeriodSeconds:    5,
				FailureThreshold: 3,
				TimeoutSeconds:   5,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "seat", MountPath: SeatMount},
				{Name: "manifest", MountPath: ManifestDir, ReadOnly: true},
				{Name: "tmp", MountPath: "/tmp"},
				{Name: "steadmesh-token", MountPath: TokenDir, ReadOnly: true},
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
		Volumes: []corev1.Volume{
			{Name: "seat", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: names.WorkspaceClaim(s.Name)}}},
			{Name: "manifest", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: names.ManifestConfigMap(s.Name)},
				DefaultMode:          ptr.To(int32(0o444)),
			}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Gi"))}}},
			projectedToken(),
		},
	}
	if rc := s.Manifest.Sandbox.RuntimeClass; rc != "" {
		pod.RuntimeClassName = ptr.To(rc)
	}
	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: meta(s, s.Name, true),
		Spec: appsv1.StatefulSetSpec{
			Replicas:             ptr.To(replicas),
			Selector:             &metav1.LabelSelector{MatchLabels: RunnerSelector(s)},
			PodManagementPolicy:  appsv1.OrderedReadyPodManagement,
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			RevisionHistoryLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: tmplLabels,
					Annotations: map[string]string{
						AnnotationConfigRevision:  s.ConfigRevision,
						v1alpha1.AnnotationSeatID: s.SeatID,
					},
				},
				Spec: pod,
			},
		},
	}, nil
}

// ProbePodName is the deterministic name of an enforcement probe Pod.
func ProbePodName(profileDigest string) string {
	d := strings.TrimPrefix(profileDigest, "sha256:")
	if len(d) > 10 {
		d = d[:10]
	}
	return "steadmesh-netprobe-" + d
}

// RenderProbePod renders the NetworkPolicy enforcement probe. It carries the
// seat's policy labels (but not the runner workload label), so the seat deny
// policy applies to it while the StatefulSet never adopts it.
func RenderProbePod(p seatruntime.EnforcementProbe, o Options) *corev1.Pod {
	o = o.withDefaults()
	s := p.Seat
	labels := SeatLabels(s)
	labels[LabelProbe] = "netpol"
	allowed := strings.TrimRight(o.PlatformURL, "/") + "/healthz"
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: ProbePodName(p.ProfileDigest), Namespace: p.Namespace, Labels: labels,
			Annotations: map[string]string{"steadmesh.io/profile-digest": p.ProfileDigest, "steadmesh.io/probe-nonce": p.Nonce},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			ActiveDeadlineSeconds:         ptr.To(int64(120)),
			TerminationGracePeriodSeconds: ptr.To(int64(5)),
			SecurityContext:               restrictedPodSecurity(),
			Containers: []corev1.Container{{
				Name:                     "netprobe",
				Image:                    o.NetprobeImage,
				ImagePullPolicy:          o.ImagePullPolicy,
				Command:                  []string{RunnerBinary, "netprobe", "--allowed", allowed, "--denied", o.NetprobeDeniedURL},
				SecurityContext:          restrictedContainerSecurity(),
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
	if rc := s.Manifest.Sandbox.RuntimeClass; rc != "" {
		pod.Spec.RuntimeClassName = ptr.To(rc)
	}
	return pod
}
