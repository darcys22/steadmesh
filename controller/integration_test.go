//go:build integration

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/go-logr/logr"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/controller/readiness"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// ---- fake platform ----------------------------------------------------------

type fakePlatform struct {
	mu        sync.Mutex
	orgs      map[string]string            // key -> id
	seats     map[string]map[string]string // org id -> seat key -> seat id
	retired   map[string]bool              // seat id
	syncs     int
	verifies  int
	deletes   []string // "<id>?retention=<r>"
	failConn  string
	probes    map[string]string // probe id -> seat id
	lastSeats map[string][]string
}

func newFakePlatform() *fakePlatform {
	return &fakePlatform{
		orgs: map[string]string{}, seats: map[string]map[string]string{}, retired: map[string]bool{},
		probes: map[string]string{}, lastSeats: map[string][]string{},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == runtimeapi.PathInternalSync:
		var req runtimeapi.SyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var m compile.Manifest
		_ = json.Unmarshal(req.Manifest, &m)
		f.syncs++
		id, ok := f.orgs[req.Key]
		if !ok {
			id = "org-" + req.Key
			f.orgs[req.Key] = id
			f.seats[id] = map[string]string{}
		}
		resp := runtimeapi.SyncResponse{OrganizationID: id, Seats: map[string]runtimeapi.SeatIdentity{}}
		var keys []string
		for k := range m.Seats {
			sid, ok := f.seats[id][k]
			if !ok {
				sid = fmt.Sprintf("seat-%s-%s", req.Key, k)
				f.seats[id][k] = sid
			}
			keys = append(keys, k)
			resp.Seats[k] = runtimeapi.SeatIdentity{SeatID: sid, ServiceAccount: names.Seat(req.Key, k), PolicyRevision: 1}
		}
		for k, sid := range f.seats[id] {
			if _, ok := m.Seats[k]; !ok && !f.retired[sid] {
				f.retired[sid] = true
				resp.Retired = append(resp.Retired, k)
			}
		}
		f.lastSeats[id] = keys
		writeJSON(w, resp)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/runtime"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, runtimeapi.PathInternalOrgs), "/runtime")
		resp := runtimeapi.RuntimeResponse{Seats: map[string]runtimeapi.SeatRuntime{}}
		for _, k := range f.lastSeats[id] {
			resp.Seats[k] = runtimeapi.SeatRuntime{SeatID: f.seats[id][k], SeatKey: k, State: "Stopped"}
		}
		writeJSON(w, resp)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/verify"):
		f.verifies++
		resp := runtimeapi.VerifyResponse{
			Connections: map[string]runtimeapi.CheckResult{"slack": {OK: true}, "tracker": {OK: true}},
			Ingress:     map[string]runtimeapi.CheckResult{"slack": {OK: true}},
			Bindings:    map[string]runtimeapi.CheckResult{"sean": {OK: true}},
		}
		if f.failConn != "" {
			resp.Connections[f.failConn] = runtimeapi.CheckResult{OK: false, Detail: "invalid_auth"}
		}
		writeJSON(w, resp)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/probe"):
		sid := strings.TrimSuffix(strings.TrimPrefix(p, runtimeapi.PathInternalSeats), "/probe")
		pid := fmt.Sprintf("probe-%d", len(f.probes)+1)
		f.probes[pid] = sid
		writeJSON(w, runtimeapi.ProbeResponse{ProbeID: pid, Status: "pending"})
	case r.Method == http.MethodGet && strings.Contains(p, "/probe/"):
		pid := p[strings.LastIndex(p, "/")+1:]
		writeJSON(w, runtimeapi.ProbeResponse{ProbeID: pid, Status: "passed", Checks: map[string]string{"tool": "ok", "workspace": "ok"}})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/fence"):
		writeJSON(w, struct{}{})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, runtimeapi.PathInternalOrgs):
		f.deletes = append(f.deletes, strings.TrimPrefix(p, runtimeapi.PathInternalOrgs)+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"code":"not_found","message":"`+r.Method+" "+p+`"}`, http.StatusNotFound)
	}
}

func (f *fakePlatform) counts() (syncs, verifies int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs, f.verifies
}

// ---- suite -------------------------------------------------------------------

var (
	k8s   client.Client
	fp    *fakePlatform
	recon *Reconcilers
)

func TestMain(m *testing.M) {
	logf.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; run bin/setup-envtest use 1.37.0 -p path --bin-dir bin/envtest")
		os.Exit(1)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "charts", "platform", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest:", err)
		os.Exit(1)
	}
	sch := k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = v1alpha1.AddToScheme(sch)

	fp = newFakePlatform()
	srv := httptest.NewServer(fp)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: sch, Cache: CacheOptions(), Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		panic(err)
	}
	recon, err = SetupReconcilers(mgr, Config{
		Platform:     platform.New(srv.URL, ""),
		Backend:      kube.Options{PlatformURL: "http://steadmesh-platform.steadmesh-system.svc:8080", ControlPlaneNamespace: "steadmesh-system"},
		PollInterval: 200 * time.Millisecond,
	})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()
	k8s, err = client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		panic(err)
	}
	// There is no kubelet in envtest: complete NetworkPolicy probe Pods as a
	// node with an enforcing CNI would.
	go completeProbePods(ctx)

	code := m.Run()
	cancel()
	srv.Close()
	_ = env.Stop()
	os.Exit(code)
}

func completeProbePods(ctx context.Context) {
	for ctx.Err() == nil {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.HasLabels{kube.LabelProbe}); err == nil {
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
					p.Status.Phase = corev1.PodSucceeded
					_ = k8s.Status().Update(ctx, p)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var pods corev1.PodList
	_ = k8s.List(context.Background(), &pods, client.HasLabels{kube.LabelProbe})
	for _, p := range pods.Items {
		t.Logf("probe pod %s/%s phase=%s nonce=%s deleting=%v", p.Namespace, p.Name, p.Status.Phase, p.Annotations["steadmesh.io/probe-nonce"], p.DeletionTimestamp != nil)
	}
	t.Fatalf("timed out waiting for %s: %v", what, err)
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const (
	cultureText = "We write things down."
	repText     = "You represent your human."
	revText     = "You review changes."
)

func setupNamespace(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bundles", Namespace: ns},
		Data: map[string]string{"culture.md": cultureText, "rep.md": repText, "reviewer.md": revText}}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
}

func ref(key, text string) string { return "configmap:bundles/" + key + "#sha256:" + sha(text) }

func orgSpec(key string) spec.OrganizationSpec {
	return spec.OrganizationSpec{
		Key: key, DisplayName: "Acme",
		CultureRefs:  []string{ref("culture.md", cultureText)},
		MemoryStores: map[string]spec.MemoryStore{"rep_sean": {}, "reviewer": {}},
		HarnessProfiles: map[string]spec.HarnessProfile{
			"fake": {Adapter: "fake", ImageDigest: "steadmesh/seat-fake:dev"},
		},
		ExecutionProfiles: map[string]spec.ExecutionProfile{"interactive": {Backend: "kubernetes"}},
		SandboxProfiles:   map[string]spec.SandboxProfile{"standard": {}},
		Connections: map[string]spec.Connection{
			"slack":   {Adapter: "slack", AccountID: "T1", SecretRef: "k8s:slack"},
			"tracker": {Adapter: "linear", SecretRef: "k8s:linear"},
		},
		Seats: map[string]spec.Seat{
			"representative_sean": {RoleRef: ref("rep.md", repText), HarnessProfile: "fake", ExecutionProfile: "interactive", SandboxProfile: "standard", PersonalMemory: "rep_sean"},
			"reviewer":            {RoleRef: ref("reviewer.md", revText), HarnessProfile: "fake", ExecutionProfile: "interactive", SandboxProfile: "standard", PersonalMemory: "reviewer"},
		},
		MessageRoutes:   map[string]spec.MessageRoute{"rep_to_reviewer": {From: "seat:representative_sean", To: "seat:reviewer", Reply: true}},
		ChannelBindings: map[string]spec.ChannelBinding{"sean": {Connection: "slack", ExternalUserID: "U0SEAN", Seat: "representative_sean"}},
	}
}

func createOrg(t *testing.T, ns string, s spec.OrganizationSpec) *v1alpha1.AgentOrganization {
	t.Helper()
	org := &v1alpha1.AgentOrganization{ObjectMeta: metav1.ObjectMeta{Name: s.Key, Namespace: ns}, Spec: v1alpha1.AgentOrganizationSpec{OrganizationSpec: s}}
	if err := k8s.Create(context.Background(), org); err != nil {
		t.Fatal(err)
	}
	return org
}

func getOrg(t *testing.T, ns, name string) *v1alpha1.AgentOrganization {
	t.Helper()
	var org v1alpha1.AgentOrganization
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &org); err != nil {
		t.Fatal(err)
	}
	return &org
}

func waitReady(t *testing.T, ns, name string) *v1alpha1.AgentOrganization {
	t.Helper()
	var org *v1alpha1.AgentOrganization
	eventually(t, 60*time.Second, "OperationalReady", func() error {
		org = getOrg(t, ns, name)
		c := meta.FindStatusCondition(org.Status.Conditions, v1alpha1.CondOperationalReady)
		if c == nil || c.Status != metav1.ConditionTrue || org.Status.ObservedGeneration != org.Generation {
			if c != nil {
				return fmt.Errorf("%s: %s: %s (observed %d/%d)", c.Status, c.Reason, c.Message, org.Status.ObservedGeneration, org.Generation)
			}
			return fmt.Errorf("no condition")
		}
		return nil
	})
	return org
}

func seatObjects(ns, orgKey, seatKey string) []client.Object {
	n := names.Seat(orgKey, seatKey)
	key := func(o client.Object, name string) client.Object { o.SetNamespace(ns); o.SetName(name); return o }
	return []client.Object{
		key(&v1alpha1.AgentSeat{}, n),
		key(&corev1.ServiceAccount{}, n),
		key(&corev1.ConfigMap{}, names.ManifestConfigMap(n)),
		key(&networkingv1.NetworkPolicy{}, n),
		key(&appsv1.StatefulSet{}, n),
		key(&corev1.PersistentVolumeClaim{}, names.WorkspaceClaim(n)),
	}
}

// ---- tests -------------------------------------------------------------------

func TestCreateOrganizationIsReadyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	ns := "org-acme"
	setupNamespace(t, ns)
	createOrg(t, ns, orgSpec("acme"))
	org := waitReady(t, ns, "acme")

	if org.Status.OrganizationID != "org-acme" || !strings.HasPrefix(org.Status.EffectiveRevision, "sha256:") {
		t.Fatalf("status = %+v", org.Status)
	}
	cd := org.Status.ConnectionDetails
	if cd == nil || cd.Representatives["sean"].Seat != "representative_sean" || cd.Representatives["sean"].SeatID == "" || cd.Representatives["sean"].Adapter != "slack" {
		t.Fatalf("connection details = %+v", cd)
	}
	for _, k := range []string{"representative_sean", "reviewer"} {
		if s := org.Status.Seats[k]; !s.Ready || s.SeatID == "" {
			t.Fatalf("seat %s summary = %+v", k, s)
		}
		for _, o := range seatObjects(ns, "acme", k) {
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatalf("%T %s: %v", o, o.GetName(), err)
			}
		}
	}
	n := names.Seat("acme", "reviewer")
	var pvc corev1.PersistentVolumeClaim
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.WorkspaceClaim(n)}, &pvc)
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("workspace PVC must have no owner reference")
	}
	var cm corev1.ConfigMap
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.ManifestConfigMap(n)}, &cm)
	instr := cm.Data[kube.InstructionsFile]
	if strings.Index(instr, cultureText) < 0 || strings.Index(instr, revText) < strings.Index(instr, cultureText) || !strings.Contains(instr, "# Organisation") {
		t.Fatalf("instructions.md = %q", instr)
	}
	var sm compile.SeatManifest
	if err := json.Unmarshal([]byte(cm.Data[kube.ManifestFile]), &sm); err != nil || sm.Key != "reviewer" {
		t.Fatalf("manifest.json: %v %+v", err, sm)
	}
	var sts appsv1.StatefulSet
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &sts)
	if src := sts.Spec.Template.Spec.Volumes; len(src) == 0 {
		t.Fatal("statefulset has no volumes")
	}
	var sa corev1.ServiceAccount
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &sa)
	if sa.Annotations[v1alpha1.AnnotationSeatID] != "seat-acme-reviewer" || *sa.AutomountServiceAccountToken {
		t.Fatalf("service account = %+v", sa.ObjectMeta)
	}

	// A03: further reconciles change nothing.
	rv := map[string]string{}
	snapshot := func() map[string]string {
		out := map[string]string{}
		o := getOrg(t, ns, "acme")
		out["org"] = o.ResourceVersion
		for _, k := range []string{"representative_sean", "reviewer"} {
			for _, obj := range seatObjects(ns, "acme", k) {
				if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				out[fmt.Sprintf("%T/%s", obj, obj.GetName())] = obj.GetResourceVersion()
			}
		}
		var seats v1alpha1.AgentSeatList
		_ = k8s.List(ctx, &seats, client.InNamespace(ns))
		out["seat-count"] = fmt.Sprint(len(seats.Items))
		var pvcs corev1.PersistentVolumeClaimList
		_ = k8s.List(ctx, &pvcs, client.InNamespace(ns))
		out["pvc-count"] = fmt.Sprint(len(pvcs.Items))
		return out
	}
	time.Sleep(time.Second) // let in-flight reconciles settle
	rv = snapshot()
	for i := 0; i < 3; i++ {
		if _, err := recon.Organization.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "acme"}}); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"representative_sean", "reviewer"} {
			if _, err := recon.Seat.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.Seat("acme", k)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(time.Second)
	after := snapshot()
	for k, v := range rv {
		if after[k] != v {
			t.Errorf("A03: %s changed on a no-op reconcile: %s -> %s", k, v, after[k])
		}
	}

	// Verify-request handshake: a nonce forces a fresh verification (A25).
	_, verifiesBefore := fp.counts()
	o := getOrg(t, ns, "acme")
	patch := client.MergeFrom(o.DeepCopy())
	o.Annotations = map[string]string{v1alpha1.AnnotationVerifyRequest: "nonce-1"}
	if err := k8s.Patch(ctx, o, patch); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "verify-observed", func() error {
		o := getOrg(t, ns, "acme")
		if o.Annotations[v1alpha1.AnnotationVerifyObserved] != "nonce-1" {
			return fmt.Errorf("observed = %q", o.Annotations[v1alpha1.AnnotationVerifyObserved])
		}
		if !condTrue(o, v1alpha1.CondOperationalReady) {
			c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondOperationalReady)
			return fmt.Errorf("not ready after verify: %s: %s", c.Reason, c.Message)
		}
		return nil
	})
	if _, v := fp.counts(); v <= verifiesBefore {
		t.Fatal("verify request must call the platform verification again")
	}
	var seat v1alpha1.AgentSeat
	_ = k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &seat)
	if seat.Status.Probe == nil || seat.Status.Probe.Nonce != "nonce-1" || seat.Status.Probe.Status != readiness.ProbePassed {
		t.Fatalf("seat probe = %+v", seat.Status.Probe)
	}

	verify := func(nonce, failing string) {
		t.Helper()
		fp.mu.Lock()
		fp.failConn = failing
		fp.mu.Unlock()
		o := getOrg(t, ns, "acme")
		patch := client.MergeFrom(o.DeepCopy())
		o.Annotations[v1alpha1.AnnotationVerifyRequest] = nonce
		if err := k8s.Patch(ctx, o, patch); err != nil {
			t.Fatal(err)
		}
	}
	// A failing optional connection (the tracker) is reported as
	// IntegrationsDegraded and never blocks readiness.
	verify("nonce-2", "tracker")
	eventually(t, 30*time.Second, "degraded integration", func() error {
		o := getOrg(t, ns, "acme")
		d := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondIntegrationsDegraded)
		if o.Annotations[v1alpha1.AnnotationVerifyObserved] != "nonce-2" || d == nil || d.Status != metav1.ConditionTrue || !strings.Contains(d.Message, "tracker") {
			return fmt.Errorf("observed %q degraded %+v", o.Annotations[v1alpha1.AnnotationVerifyObserved], d)
		}
		if !condTrue(o, v1alpha1.CondOperationalReady) {
			return errors.New("an optional connection blocked readiness")
		}
		return nil
	})
	// A failing required connection is reported after a fresh verification (A02/A25).
	verify("nonce-3", "slack")
	eventually(t, 30*time.Second, "blocked condition", func() error {
		o := getOrg(t, ns, "acme")
		c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondOperationalReady)
		if o.Annotations[v1alpha1.AnnotationVerifyObserved] != "nonce-3" || c.Status != metav1.ConditionFalse {
			return fmt.Errorf("observed %q ready %s", o.Annotations[v1alpha1.AnnotationVerifyObserved], c.Status)
		}
		if c.Reason != v1alpha1.CondConnectionsAuthenticated || !strings.Contains(c.Message, "slack") {
			return fmt.Errorf("reason %s: %s", c.Reason, c.Message)
		}
		return nil
	})
	fp.mu.Lock()
	fp.failConn = ""
	fp.mu.Unlock()
}

func condTrue(o *v1alpha1.AgentOrganization, t string) bool {
	c := meta.FindStatusCondition(o.Status.Conditions, t)
	return c != nil && c.Status == metav1.ConditionTrue
}

func TestSeatRemovalRetiresAndRetainsWorkspace(t *testing.T) {
	ctx := context.Background()
	ns := "org-retire"
	setupNamespace(t, ns)
	s := orgSpec("retire")
	createOrg(t, ns, s)
	waitReady(t, ns, "retire")

	o := getOrg(t, ns, "retire")
	delete(o.Spec.Seats, "reviewer")
	delete(o.Spec.MessageRoutes, "rep_to_reviewer")
	delete(o.Spec.MemoryStores, "reviewer")
	if err := k8s.Update(ctx, o); err != nil {
		t.Fatal(err)
	}
	n := names.Seat("retire", "reviewer")
	eventually(t, 30*time.Second, "reviewer AgentSeat removed", func() error {
		var seat v1alpha1.AgentSeat
		err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &seat)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("seat still present (state %s, retired %t)", seat.Status.ExecutionState, seat.Spec.Retired)
	})
	for _, obj := range seatObjects(ns, "retire", "reviewer")[1:5] {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s should be removed: %v", obj, obj.GetName(), err)
		}
	}
	var pvc corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.WorkspaceClaim(n)}, &pvc); err != nil {
		t.Fatalf("retired seat's workspace must be retained: %v", err)
	}
	fp.mu.Lock()
	retired := fp.retired["seat-retire-reviewer"]
	fp.mu.Unlock()
	if !retired {
		t.Fatal("platform sync must retire the identity")
	}
	org := waitReady(t, ns, "retire")
	if _, ok := org.Status.Seats["reviewer"]; ok {
		t.Fatal("retired seat must leave the status summary")
	}
}

func TestInvalidSpecIsNotConfigured(t *testing.T) {
	ns := "org-invalid"
	setupNamespace(t, ns)
	s := orgSpec("invalid")
	seat := s.Seats["reviewer"]
	seat.HarnessProfile = "missing"
	s.Seats["reviewer"] = seat
	createOrg(t, ns, s)
	eventually(t, 20*time.Second, "Configured=False", func() error {
		o := getOrg(t, ns, "invalid")
		c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondConfigured)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InvalidSpec" || !strings.Contains(c.Message, "seats.reviewer.harness_profile") {
			return fmt.Errorf("configured = %+v", c)
		}
		r := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondOperationalReady)
		if r == nil || r.Status != metav1.ConditionFalse || r.Reason != v1alpha1.CondConfigured || o.Status.ObservedGeneration != o.Generation {
			return fmt.Errorf("operational = %+v", r)
		}
		return nil
	})
	var seats v1alpha1.AgentSeatList
	_ = k8s.List(context.Background(), &seats, client.InNamespace(ns))
	if len(seats.Items) != 0 {
		t.Fatal("no seats for an invalid specification")
	}
}

func TestInstructionDigestMismatch(t *testing.T) {
	ns := "org-digest"
	setupNamespace(t, ns)
	s := orgSpec("digest")
	s.CultureRefs = []string{"configmap:bundles/culture.md#sha256:" + sha("tampered")}
	createOrg(t, ns, s)
	eventually(t, 20*time.Second, "digest mismatch", func() error {
		c := meta.FindStatusCondition(getOrg(t, ns, "digest").Status.Conditions, v1alpha1.CondConfigured)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonInstructionDigestMismatch {
			return fmt.Errorf("configured = %+v", c)
		}
		return nil
	})
}

func TestUnsupportedRuntimeClassBlocks(t *testing.T) {
	ctx := context.Background()
	ns := "org-rc"
	setupNamespace(t, ns)
	s := orgSpec("rc")
	s.SandboxProfiles["standard"] = spec.SandboxProfile{RuntimeClass: "kata-missing"}
	createOrg(t, ns, s)
	n := names.Seat("rc", "reviewer")
	eventually(t, 30*time.Second, "seat Blocked", func() error {
		var seat v1alpha1.AgentSeat
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &seat); err != nil {
			return err
		}
		c := meta.FindStatusCondition(seat.Status.Conditions, readiness.SeatBackendValid)
		if seat.Status.ExecutionState != v1alpha1.StateBlocked || c == nil || c.Reason != "Misconfigured" {
			return fmt.Errorf("state %s cond %+v", seat.Status.ExecutionState, c)
		}
		return nil
	})
	var sts appsv1.StatefulSet
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &sts); !apierrors.IsNotFound(err) {
		t.Fatalf("no StatefulSet may exist for a blocked sandbox: %v", err)
	}
	eventually(t, 20*time.Second, "SandboxEnforced=False", func() error {
		o := getOrg(t, ns, "rc")
		c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.CondSandboxEnforced)
		if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "kata-missing") {
			return fmt.Errorf("sandbox = %+v", c)
		}
		if condTrue(o, v1alpha1.CondOperationalReady) {
			return fmt.Errorf("must not be ready")
		}
		return nil
	})
}

func TestDeleteWithRetentionKeepsWorkspaces(t *testing.T) {
	ctx := context.Background()
	ns := "org-delete"
	setupNamespace(t, ns)
	createOrg(t, ns, orgSpec("del"))
	org := waitReady(t, ns, "del")
	if err := k8s.Delete(ctx, org); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "organisation deleted", func() error {
		var o v1alpha1.AgentOrganization
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(org), &o); apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("still present")
	})
	var seats v1alpha1.AgentSeatList
	_ = k8s.List(ctx, &seats, client.InNamespace(ns))
	if len(seats.Items) != 0 {
		t.Fatalf("seats remain: %d", len(seats.Items))
	}
	var sts appsv1.StatefulSetList
	_ = k8s.List(ctx, &sts, client.InNamespace(ns))
	if len(sts.Items) != 0 {
		t.Fatal("runtime must be removed")
	}
	var pvcs corev1.PersistentVolumeClaimList
	_ = k8s.List(ctx, &pvcs, client.InNamespace(ns))
	if len(pvcs.Items) != 2 {
		t.Fatalf("workspaces must be retained (A19): %d", len(pvcs.Items))
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	found := false
	for _, d := range fp.deletes {
		if d == "org-del?retention=retain" {
			found = true
		}
	}
	if !found {
		t.Fatalf("platform delete calls = %v", fp.deletes)
	}
}
