package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/spec"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

func testSeat() *seatruntime.Seat {
	name := names.Seat("acme", "reviewer")
	return &seatruntime.Seat{
		Namespace: "org-acme", Name: name, OrgKey: "acme", OrgID: "org-1", SeatKey: "reviewer", SeatID: "seat-1",
		ConfigRevision: "sha256:abc",
		Owner:          &metav1.OwnerReference{APIVersion: "steadmesh.io/v1alpha1", Kind: "AgentSeat", Name: name, UID: "uid-1", Controller: ptr.To(true)},
		Manifest: compile.SeatManifest{
			Key: "reviewer", HarnessProfile: "fake",
			Harness:   spec.HarnessProfile{Adapter: "fake", ImageDigest: "steadmesh/seat-fake:dev"},
			Execution: spec.ExecutionProfile{Backend: "kubernetes", IdlePolicy: "warm_then_stop", CPURequest: "250m", CPULimit: "2", MemoryRequest: "512Mi", MemoryLimit: "2Gi", WorkspaceSizeGB: 7, StorageClass: "fast"},
			Sandbox:   spec.SandboxProfile{NetworkPolicyRef: "deny_all_except_platform", RuntimeClass: "gvisor"},
		},
	}
}

func testOptions() Options {
	return Options{PlatformURL: "http://steadmesh-platform.steadmesh-system.svc:8080", ControlPlaneNamespace: "steadmesh-system"}
}

func render(t *testing.T) *appsv1.StatefulSet {
	t.Helper()
	sts, err := RenderStatefulSet(testSeat(), testOptions(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return sts
}

func TestStatefulSetPodContract(t *testing.T) {
	sts := render(t)
	s := testSeat()
	if *sts.Spec.Replicas != 1 || sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatalf("replicas/strategy = %d/%s", *sts.Spec.Replicas, sts.Spec.UpdateStrategy.Type)
	}
	if len(sts.OwnerReferences) != 1 || sts.OwnerReferences[0].Kind != "AgentSeat" {
		t.Fatalf("owner refs = %v", sts.OwnerReferences)
	}
	pod := sts.Spec.Template.Spec
	if pod.ServiceAccountName != s.Name || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("service account token must not be automounted")
	}
	if *pod.TerminationGracePeriodSeconds != 300 {
		t.Fatalf("grace = %d", *pod.TerminationGracePeriodSeconds)
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "gvisor" {
		t.Fatal("runtime class not propagated")
	}
	psc := pod.SecurityContext
	if !*psc.RunAsNonRoot || *psc.RunAsUser != 1000 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context = %+v", psc)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Command[0] != "/usr/local/bin/seat-runner" || c.Image != "steadmesh/seat-fake:dev" || c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("container = %v %s %s", c.Command, c.Image, c.ImagePullPolicy)
	}
	sc := c.SecurityContext
	if !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || !*sc.RunAsNonRoot || *sc.RunAsUser != 1000 ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("container security context = %+v", sc)
	}
	if c.Resources.Limits.Cpu().String() != "2" || c.Resources.Limits.Memory().String() != "2Gi" || c.Resources.Requests.Cpu().String() != "250m" {
		t.Fatalf("resources = %+v", c.Resources)
	}
	if c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.IntVal != 8081 ||
		c.LivenessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.HTTPGet.Port.IntVal != 8081 {
		t.Fatal("probes do not match the contract")
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	for k, v := range map[string]string{
		"STEADMESH_PLATFORM_URL": "http://steadmesh-platform.steadmesh-system.svc:8080", "STEADMESH_TOKEN_FILE": "/var/run/steadmesh/token",
		"STEADMESH_ORG_ID": "org-1", "STEADMESH_SEAT_ID": "seat-1", "STEADMESH_SEAT_KEY": "reviewer", "STEADMESH_CONFIG_REVISION": "sha256:abc",
		"STEADMESH_HARNESS": "fake", "STEADMESH_MANIFEST_DIR": "/etc/steadmesh/manifest",
	} {
		if env[k].Value != v {
			t.Errorf("env %s = %q, want %q", k, env[k].Value, v)
		}
	}
	if f := env["POD_UID"].ValueFrom; f == nil || f.FieldRef.FieldPath != "metadata.uid" {
		t.Error("POD_UID must come from the downward API")
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v
	}
	if v := vols[mounts["/seat"].Name]; v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "ws-"+s.Name {
		t.Fatalf("/seat volume = %+v", v)
	}
	if m := mounts["/etc/steadmesh/manifest"]; !m.ReadOnly || vols[m.Name].ConfigMap == nil || vols[m.Name].ConfigMap.Name != s.Name+"-manifest" {
		t.Fatal("manifest must be a read-only ConfigMap mount")
	}
	if vols[mounts["/tmp"].Name].EmptyDir == nil {
		t.Fatal("/tmp must be an emptyDir")
	}
	tok := vols[mounts["/var/run/steadmesh"].Name]
	if tok.Projected == nil || len(tok.Projected.Sources) != 1 {
		t.Fatal("token must be a projected volume")
	}
	sat := tok.Projected.Sources[0].ServiceAccountToken
	if sat.Audience != v1alpha1.GatewayAudience || *sat.ExpirationSeconds != 3600 || sat.Path != "token" {
		t.Fatalf("token projection = %+v", sat)
	}
	// Selector and template labels: the runner workload label prevents adoption of probe Pods.
	if sts.Spec.Selector.MatchLabels[LabelWorkload] != WorkloadRunner || sts.Spec.Template.Labels[LabelWorkload] != WorkloadRunner {
		t.Fatal("selector must include the runner workload label")
	}
	if sts.Spec.Template.Annotations[AnnotationConfigRevision] != "sha256:abc" {
		t.Fatal("template must record the config revision")
	}
}

func TestStatefulSetNoRuntimeClassWhenUnset(t *testing.T) {
	s := testSeat()
	s.Manifest.Sandbox.RuntimeClass = ""
	sts, err := RenderStatefulSet(s, testOptions(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.RuntimeClassName != nil || *sts.Spec.Replicas != 0 {
		t.Fatal("unexpected runtime class or replicas")
	}
	if _, err := RenderStatefulSet(s, testOptions(), 2); err == nil {
		t.Fatal("replicas 2 must be rejected")
	}
}

func TestStatefulSetRequiresLimits(t *testing.T) {
	s := testSeat()
	s.Manifest.Execution.MemoryLimit = ""
	_, err := RenderStatefulSet(s, testOptions(), 1)
	if seatruntime.KindOf(err) != seatruntime.Misconfigured {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceClaimHasNoOwner(t *testing.T) {
	pvc := RenderWorkspaceClaim(testSeat())
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("the workspace PVC must not have an owner reference (§5.5)")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Fatal("storage class from the execution profile")
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "7Gi" {
		t.Fatalf("size = %s", q.String())
	}
	if pvc.Annotations[v1alpha1.AnnotationSeatID] != "seat-1" {
		t.Fatal("claim must record the seat identity")
	}
	s := testSeat()
	s.Manifest.Execution.StorageClass = ""
	if RenderWorkspaceClaim(s).Spec.StorageClassName != nil {
		t.Fatal("empty storage class must use the cluster default")
	}
}

func TestServiceAccount(t *testing.T) {
	sa := RenderServiceAccount(testSeat())
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken must be false")
	}
	if sa.Annotations[v1alpha1.AnnotationSeatID] != "seat-1" || sa.Annotations[v1alpha1.AnnotationOrgID] != "org-1" {
		t.Fatalf("annotations = %v", sa.Annotations)
	}
	if sa.Labels[v1alpha1.LabelComponent] != "seat" || sa.Labels[v1alpha1.LabelSeat] != "reviewer" || sa.Labels[v1alpha1.LabelOrganization] != "acme" {
		t.Fatalf("labels = %v", sa.Labels)
	}
	if len(sa.OwnerReferences) != 1 {
		t.Fatal("service account is owned by the AgentSeat")
	}
}

func TestNetworkPolicyShape(t *testing.T) {
	np := RenderNetworkPolicy(testSeat(), testOptions())
	if len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Ingress) != 0 {
		t.Fatal("ingress must be denied and both policy types declared")
	}
	if np.Spec.PodSelector.MatchLabels[v1alpha1.LabelSeat] != "reviewer" || np.Spec.PodSelector.MatchLabels[v1alpha1.LabelOrganization] != "acme" {
		t.Fatalf("pod selector = %v", np.Spec.PodSelector)
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d", len(np.Spec.Egress))
	}
	plat := np.Spec.Egress[0]
	if len(plat.To) != 1 || plat.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "steadmesh-system" ||
		plat.To[0].PodSelector.MatchLabels[v1alpha1.LabelComponent] != "platform" || plat.To[0].IPBlock != nil {
		t.Fatalf("platform peer = %+v", plat.To)
	}
	if len(plat.Ports) != 1 || *plat.Ports[0].Protocol != corev1.ProtocolTCP || plat.Ports[0].Port.IntVal != 8080 {
		t.Fatalf("platform ports = %+v", plat.Ports)
	}
	dns := np.Spec.Egress[1]
	protos := map[corev1.Protocol]bool{}
	for _, p := range dns.Ports {
		if p.Port.IntVal != 53 {
			t.Fatalf("dns port = %v", p.Port)
		}
		protos[*p.Protocol] = true
	}
	if !protos[corev1.ProtocolUDP] || !protos[corev1.ProtocolTCP] {
		t.Fatal("DNS must allow UDP and TCP 53")
	}
	var _ networkingv1.NetworkPolicy = *np
}

func TestProbePodMatchesPolicyButNotStatefulSet(t *testing.T) {
	s := testSeat()
	p := RenderProbePod(seatruntime.EnforcementProbe{Namespace: s.Namespace, ProfileDigest: "sha256:0123456789abcdef", Seat: s, Nonce: "n1"}, Options{NetprobeImage: "steadmesh/seat-fake:dev"})
	np := RenderNetworkPolicy(s, testOptions())
	for k, v := range np.Spec.PodSelector.MatchLabels {
		if p.Labels[k] != v {
			t.Fatalf("probe pod lacks policy label %s", k)
		}
	}
	sts := render(t)
	if _, ok := p.Labels[LabelWorkload]; ok {
		t.Fatal("probe pod must not match the StatefulSet selector")
	}
	_ = sts
	c := p.Spec.Containers[0]
	if c.Command[0] != RunnerBinary || c.Command[1] != "netprobe" || c.Command[2] != "--allowed" || c.Command[3] != "http://steadmesh-platform.steadmesh-system.svc:8080/healthz" || c.Command[4] != "--denied" {
		t.Fatalf("command = %v", c.Command)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever || *p.Spec.AutomountServiceAccountToken {
		t.Fatal("probe pod must not restart or mount tokens")
	}
	if p.Name != "steadmesh-netprobe-0123456789" {
		t.Fatalf("name = %s", p.Name)
	}
}

func TestToApplyDropsStatusAndNulls(t *testing.T) {
	u, err := toApply(render(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Object["status"]; ok {
		t.Fatal("status must not be applied")
	}
	md := u.Object["metadata"].(map[string]any)
	if _, ok := md["creationTimestamp"]; ok {
		t.Fatal("null creationTimestamp must be pruned")
	}
	if u.GetKind() != "StatefulSet" || u.GetAPIVersion() != "apps/v1" {
		t.Fatal("type meta required for apply")
	}
}

func TestRuntimeClassProvides(t *testing.T) {
	cases := map[string][]string{"runsc": {"gvisor"}, "kata-qemu": {"microvm"}, "runc": nil}
	for h, want := range cases {
		got := runtimeClassProvides(&nodev1.RuntimeClass{Handler: h})
		if len(got) != len(want) || (len(want) > 0 && got[0] != want[0]) {
			t.Errorf("%s: got %v want %v", h, got, want)
		}
	}
	got := runtimeClassProvides(&nodev1.RuntimeClass{Handler: "custom", ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationProvides: "microvm"}}})
	if len(got) != 1 || got[0] != "microvm" {
		t.Errorf("annotation: %v", got)
	}
}

func TestAccessShapesNetworkPolicyAndEnv(t *testing.T) {
	o := testOptions()
	o.EgressURL = "http://steadmesh-egress.steadmesh-system.svc:3128"
	plain := RenderNetworkPolicy(testSeat(), o)
	if len(plain.Spec.Egress) != 2 {
		t.Fatalf("a seat without access has %d egress rules, want platform and DNS only", len(plain.Spec.Egress))
	}
	s := testSeat()
	s.Manifest.Access = &access.SeatAccess{Profiles: []string{"p"},
		Egress:  []access.EgressRule{{Host: "github.com", Ports: []int{443}}},
		Network: []access.NetworkRule{{CIDR: "10.0.5.0/24", Ports: []int64{5432}, Protocol: "tcp"}, {CIDR: "10.0.6.0/24", Protocol: "udp"}}}
	np := RenderNetworkPolicy(s, o)
	if len(np.Spec.Egress) != 5 {
		t.Fatalf("egress rules %+v", np.Spec.Egress)
	}
	gw := np.Spec.Egress[2]
	if gw.To[0].PodSelector.MatchLabels[v1alpha1.LabelComponent] != ComponentEgress || gw.Ports[0].Port.IntValue() != 3128 {
		t.Fatalf("gateway rule %+v", gw)
	}
	if r := np.Spec.Egress[3]; r.To[0].IPBlock.CIDR != "10.0.5.0/24" || r.Ports[0].Port.IntValue() != 5432 || *r.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("cidr rule %+v", r)
	}
	if r := np.Spec.Egress[4]; r.Ports[0].Port != nil || *r.Ports[0].Protocol != corev1.ProtocolUDP {
		t.Fatalf("all-ports rule %+v", r)
	}
	sts, err := RenderStatefulSet(s, o, 1)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["STEADMESH_EGRESS_URL"] != o.EgressURL {
		t.Fatalf("egress URL %q", env["STEADMESH_EGRESS_URL"])
	}
	// Without the gateway, a seat that needs it is refused.
	b := &Backend{o: testOptions()}
	if err := b.Validate(context.Background(), s); err == nil || !strings.Contains(err.Error(), "enable_egress") {
		t.Fatalf("validate without gateway: %v", err)
	}
}
