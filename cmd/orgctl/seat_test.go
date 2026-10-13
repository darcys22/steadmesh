package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func seatFixture(t *testing.T) (client.Client, seatOptions, *v1alpha1.AgentSeat) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	o := seatOptions{kctx: "production", namespace: "team", organization: "agents", key: "lead_engineer", output: "text", interval: time.Millisecond}
	org := &v1alpha1.AgentOrganization{
		ObjectMeta: metav1.ObjectMeta{Name: o.organization, Namespace: o.namespace, UID: "org-uid"},
		Spec:       v1alpha1.AgentOrganizationSpec{OrganizationSpec: spec.OrganizationSpec{Key: "org-key"}},
		Status:     v1alpha1.AgentOrganizationStatus{OrganizationID: "org-id"},
	}
	seat := &v1alpha1.AgentSeat{
		ObjectMeta: metav1.ObjectMeta{Name: names.Seat(org.Spec.Key, o.key), Namespace: o.namespace, UID: "seat-uid", ResourceVersion: "7", Generation: 3},
		Spec: v1alpha1.AgentSeatSpec{OrganizationName: org.Name, OrganizationID: "org-id", SeatKey: o.key, SeatID: "seat-id",
			ConfigRevision: "config-3", HarnessProfile: "codex", ExecutionProfile: "standard", SandboxProfile: "restricted",
			Workspace: v1alpha1.WorkspaceRef{ClaimName: "workspace", Shared: []string{"shared-workspace"}},
			Tools:     []v1alpha1.ToolCapability{{Resource: "messages", Operations: []string{"send"}}}},
		Status: v1alpha1.AgentSeatStatus{ExecutionState: v1alpha1.StateExecuting, ObservedGeneration: 2, AdoptedRevision: "config-2",
			LeaseGeneration: 4, LatestCheckpoint: "portable handoff", LastError: "redacted error", BackendRef: "statefulset/runner",
			Probe:      &v1alpha1.SeatProbeStatus{ID: "probe", Status: "passed", Revision: "config-2"},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ProbePending", Message: "waiting for probe", LastTransitionTime: metav1.NewTime(time.Unix(10, 0))}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentSeat{}, &v1alpha1.AgentOrganization{}).WithObjects(org, seat).Build()
	return c, o, seat
}

func TestSeatCommandArgumentsFailBeforeClusterAccess(t *testing.T) {
	t.Setenv("STEADMESH_KUBE_CONTEXT", "")
	for _, cmd := range []string{"inspect", "watch", "pause", "resume"} {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"context", nil, "--context is required"},
			{"scope", []string{"--context", "prod", "--seat", "lead"}, "require --namespace, --name and --seat"},
			{"seat", []string{"--context", "prod", "--namespace", "team", "--name", "org"}, "stable seat key"},
			{"positional", []string{"lead", "--context", "prod"}, "unexpected positional"},
			{"interval", []string{"--context", "prod", "--namespace", "team", "--name", "org", "--seat", "lead", "--interval", "0"}, "--interval"},
			{"output", []string{"--context", "prod", "--namespace", "team", "--name", "org", "--seat", "lead", "--output", "yaml"}, "--output"},
		} {
			t.Run(cmd+"/"+tc.name, func(t *testing.T) {
				args := append([]string{cmd, "--kubeconfig", "/does-not-exist"}, tc.args...)
				if err := run(args); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("run(%q) = %v, want %q", args, err, tc.want)
				}
			})
		}
	}
	if err := run([]string{"paus"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command: %v", err)
	}
}

func TestResolveSeatStableKeyAndScope(t *testing.T) {
	c, o, expected := seatFixture(t)
	// An identically keyed seat in another namespace must never be selected.
	other := expected.DeepCopy()
	other.Namespace = "other"
	other.ResourceVersion = ""
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSeat(context.Background(), c, o)
	if err != nil || got.Name != expected.Name || got.Namespace != o.namespace {
		t.Fatalf("resolve = %#v, %v", got, err)
	}
	o.key = expected.Name // Kubernetes object names are not accepted as seat keys.
	if _, err := resolveSeat(context.Background(), c, o); err == nil || !strings.Contains(err.Error(), "check the key") {
		t.Fatalf("object name should not resolve as a seat key: %v", err)
	}
}

func TestResolveSeatRejectsUnreadyOrMismatchedIdentity(t *testing.T) {
	for _, field := range []string{"organisation name", "organisation ID", "seat key", "seat ID", "organisation unready"} {
		t.Run(field, func(t *testing.T) {
			c, o, seat := seatFixture(t)
			switch field {
			case "organisation name":
				seat.Spec.OrganizationName = "different"
			case "organisation ID":
				seat.Spec.OrganizationID = "different"
			case "seat key":
				seat.Spec.SeatKey = "different"
			case "seat ID":
				seat.Spec.SeatID = ""
			case "organisation unready":
				var org v1alpha1.AgentOrganization
				if err := c.Get(context.Background(), types.NamespacedName{Namespace: o.namespace, Name: o.organization}, &org); err != nil {
					t.Fatal(err)
				}
				org.Status.OrganizationID = ""
				if err := c.Status().Update(context.Background(), &org); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Update(context.Background(), seat); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveSeat(context.Background(), c, o); err == nil {
				t.Fatal("unexpectedly resolved mismatched/unready identity")
			}
		})
	}
}

type patchRecorder struct {
	client.Client
	patches [][]byte
	err     error
}

func (c *patchRecorder) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	c.patches = append(c.patches, data)
	if c.err != nil {
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestPauseResumePatchOnlyAdminControl(t *testing.T) {
	base, o, original := seatFixture(t)
	c := &patchRecorder{Client: base}
	for _, cmd := range []string{"pause", "pause", "resume", "resume"} {
		var out bytes.Buffer
		if err := seatCommand(context.Background(), c, cmd, o, &out); err != nil {
			t.Fatal(err)
		}
		want := "Admin suspended"
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "production") {
			t.Fatalf("missing requested state/context: %s", out.String())
		}
	}
	if len(c.patches) != 2 {
		t.Fatalf("idempotent changes should patch twice, got %d", len(c.patches))
	}
	for i, data := range c.patches {
		var p map[string]json.RawMessage
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		var spec map[string]any
		var metadata map[string]any
		_ = json.Unmarshal(p["spec"], &spec)
		_ = json.Unmarshal(p["metadata"], &metadata)
		if len(p) != 2 || len(spec) != 1 || spec["adminSuspended"] != (i == 0) || len(metadata) != 1 || metadata["resourceVersion"] == "" {
			t.Fatalf("unexpected patch: %s", data)
		}
	}
	got, err := resolveSeat(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.AdminSuspended || got.Spec.ConfigRevision != original.Spec.ConfigRevision || got.Spec.Workspace.ClaimName != original.Spec.Workspace.ClaimName || got.Status.LeaseGeneration != original.Status.LeaseGeneration || got.Status.ExecutionState != original.Status.ExecutionState {
		t.Fatalf("unrelated spec/status changed: %#v", got)
	}
}

func TestSuspensionRejectsConcurrentUpdates(t *testing.T) {
	c, o, _ := seatFixture(t)
	stale, err := resolveSeat(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	current := stale.DeepCopy()
	current.Spec.ConfigRevision = "concurrent-new-config"
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := setSeatSuspended(context.Background(), c, stale, true); !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "inspect it and retry") {
		t.Fatalf("want helpful conflict, got %v", err)
	}
	got, err := resolveSeat(context.Background(), c, o)
	if err != nil || got.Spec.AdminSuspended || got.Spec.ConfigRevision != current.Spec.ConfigRevision {
		t.Fatalf("concurrent update overwritten: %#v, %v", got, err)
	}
}

func TestSuspensionReportsRBACAndExactContext(t *testing.T) {
	base, o, _ := seatFixture(t)
	c := &patchRecorder{Client: base, err: apierrors.NewForbidden(schema.GroupResource{Group: "steadmesh.io", Resource: "agentseats"}, "seat", errors.New("denied"))}
	var out bytes.Buffer
	err := seatCommand(context.Background(), c, "pause", o, &out)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("want forbidden, got %v", err)
	}
	for _, want := range []string{"production", "team/agents", "lead_engineer", "patch permission"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("failed mutation printed success: %s", out.String())
	}
}

func TestInspectSeatTextAndJSON(t *testing.T) {
	c, o, seat := seatFixture(t)
	var out bytes.Buffer
	if err := seatCommand(context.Background(), c, "inspect", o, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{o.kctx, o.key, seat.Name, "observed 2", "config-3", "config-2", "portable handoff", "ProbePending", "redacted error"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	o.output = "json"
	seat.Annotations = map[string]string{"unrelated": "annotation-not-to-print"}
	out.Reset()
	if err := printSeat(&out, o, seat); err != nil {
		t.Fatal(err)
	}
	var view seatInspection
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Context != o.kctx || view.Spec.SeatKey != o.key || view.Status.LeaseGeneration != 4 || strings.Contains(out.String(), "annotation-not-to-print") {
		t.Fatalf("unexpected inspection: %s", out.String())
	}
}

type sequenceClient struct {
	client.Client
	get func(client.Object) error
}

func (c sequenceClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return c.get(obj)
}

func TestWatchSeatPrintsChangesOnlyAndCancels(t *testing.T) {
	_, o, seat := seatFixture(t)
	o.output = "json"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	c := sequenceClient{get: func(obj client.Object) error {
		reads++
		cur := seat.DeepCopy()
		cur.ResourceVersion = fmt.Sprint(reads + 20) // metadata-only changes are quiet
		if reads >= 2 {
			cur.Status.ExecutionState = v1alpha1.StateBlocked
			cur.Spec.AdminSuspended = true
		}
		if reads == 3 {
			cancel()
		}
		*obj.(*v1alpha1.AgentSeat) = *cur
		return nil
	}}
	var out bytes.Buffer
	if err := watchSeat(ctx, c, o, seat, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"adminSuspended":true`) || !strings.Contains(lines[1], `"executionState":"Blocked"`) {
		t.Fatalf("unexpected updates: %s", out.String())
	}
}

func TestWatchSeatCancellationInterruptsInterval(t *testing.T) {
	_, o, seat := seatFixture(t)
	o.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := sequenceClient{get: func(client.Object) error { t.Fatal("canceled watch should not read"); return nil }}
	if err := watchSeat(ctx, c, o, seat, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestWatchSeatStopsOnReplacementOrReadFailure(t *testing.T) {
	for _, kind := range []string{"replacement", "forbidden", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			_, o, seat := seatFixture(t)
			c := sequenceClient{get: func(obj client.Object) error {
				switch kind {
				case "forbidden":
					return apierrors.NewForbidden(schema.GroupResource{Resource: "agentseats"}, seat.Name, errors.New("denied"))
				case "deleted":
					return apierrors.NewNotFound(schema.GroupResource{Resource: "agentseats"}, seat.Name)
				default:
					*obj.(*v1alpha1.AgentSeat) = *seat.DeepCopy()
					obj.SetUID("new-seat-uid")
					return nil
				}
			}}
			if err := watchSeat(context.Background(), c, o, seat, io.Discard); err == nil {
				t.Fatal("watch silently continued after identity/access loss")
			}
		})
	}
}

func TestClientHonorsExactContextInsteadOfAmbient(t *testing.T) {
	var ambientCalls atomic.Int32
	ambient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ambientCalls.Add(1)
		http.Error(w, "ambient cluster must not be contacted", http.StatusInternalServerError)
	}))
	defer ambient.Close()
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			fmt.Fprint(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"steadmesh.io","versions":[{"groupVersion":"steadmesh.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"steadmesh.io/v1alpha1","version":"v1alpha1"}}]}`)
		case "/apis/steadmesh.io/v1alpha1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"steadmesh.io/v1alpha1","resources":[{"name":"agentorganizations","singularName":"agentorganization","namespaced":true,"kind":"AgentOrganization","verbs":["get"]}]}`)
		case "/apis/steadmesh.io/v1alpha1/namespaces/team/agentorganizations/agents":
			fmt.Fprint(w, `{"kind":"AgentOrganization","apiVersion":"steadmesh.io/v1alpha1","metadata":{"name":"agents","namespace":"team"},"status":{"organizationID":"selected-org"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer selected.Close()
	cfg := clientcmdapi.Config{CurrentContext: "ambient",
		Clusters: map[string]*clientcmdapi.Cluster{"ambient": {Server: ambient.URL}, "selected": {Server: selected.URL}},
		Contexts: map[string]*clientcmdapi.Context{"ambient": {Cluster: "ambient"}, "selected": {Cluster: "selected"}},
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	c, err := newClient(path, "selected")
	if err != nil {
		t.Fatal(err)
	}
	var org v1alpha1.AgentOrganization
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "agents"}, &org); err != nil {
		t.Fatal(err)
	}
	if org.Status.OrganizationID != "selected-org" || ambientCalls.Load() != 0 {
		t.Fatalf("wrong context: org=%s ambientCalls=%d", org.Status.OrganizationID, ambientCalls.Load())
	}
	if _, err := newClient(path, "missing-context"); err == nil {
		t.Fatal("unknown context should fail instead of falling back")
	}
	if ambientCalls.Load() != 0 {
		t.Fatal("unknown context contacted ambient cluster")
	}
}
