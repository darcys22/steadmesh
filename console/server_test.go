package console

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const (
	orgID  = "11111111-1111-1111-1111-111111111111"
	leadID = "22222222-2222-2222-2222-222222222222"
	repID  = "33333333-3333-3333-3333-333333333333"
	runID  = "44444444-4444-4444-4444-444444444444"
	convID = "55555555-5555-5555-5555-555555555555"
)

type fakePlatform struct {
	mu       sync.Mutex
	activity []runtimeapi.ConsoleActivityItem
}

func (f *fakePlatform) Organizations(context.Context) ([]runtimeapi.ConsoleOrganization, error) {
	o, _ := f.Organization(context.Background(), orgID)
	return []runtimeapi.ConsoleOrganization{*o}, nil
}

func (f *fakePlatform) Organization(_ context.Context, id string) (*runtimeapi.ConsoleOrganization, error) {
	if id != orgID {
		return nil, ErrNotFound
	}
	ok := true
	return &runtimeapi.ConsoleOrganization{ID: orgID, Key: "acme", Namespace: "acme", DisplayName: "Acme", Revision: "sha256:abcdef0123",
		Teams: []string{"engineering"},
		Seats: []runtimeapi.ConsoleSeatConfig{
			{SeatID: leadID, OrganizationID: orgID, Key: "lead", DisplayName: "Engineering Lead", RoleRef: "lead", Teams: []string{"engineering"},
				ConfigRevision: "rev-2", PersistentWorkspace: true, Capabilities: []runtimeapi.ConsoleCapability{{Resource: "connection:tracker", Operations: []string{"task.write"}}}},
			{SeatID: repID, OrganizationID: orgID, Key: "rep_a", DisplayName: "Alice's rep", RoleRef: "representative", IsRepresentative: true,
				ConfigRevision: "rev-2", SendTo: []string{"lead"}},
		},
		Connections: []runtimeapi.ConsoleConnection{{Key: "tracker", Adapter: "linear", Required: ok, SecretKind: "vault", SecretPath: "steadmesh/linear",
			Check: &runtimeapi.ConsoleCheck{OK: false, Detail: "401 from Linear", CheckedAt: now.Add(-time.Minute),
				Credential: runtimeapi.CredentialStatus{State: runtimeapi.CredentialReplacementRejected, Error: "replacement credential (secret version 4) rejected: 401"}}}},
	}, nil
}

func (f *fakePlatform) Seats(context.Context, string) ([]runtimeapi.ConsoleSeatStatus, error) {
	progress := now.Add(-30 * time.Second)
	return []runtimeapi.ConsoleSeatStatus{
		{SeatRuntime: runtimeapi.SeatRuntime{SeatID: leadID, SeatKey: "lead", State: "Executing", LeaseHolder: "pod-uid-1",
			LeaseExpiresAt: now.Add(time.Minute), LeaseGeneration: 3, AdoptedRevision: "rev-2"}, LastProgressAt: &progress,
			Current:           &runtimeapi.ConsoleExecution{ID: runID, SeatKey: "lead", TriggerSummary: "please build the login page", StartedAt: now.Add(-time.Minute)},
			UnknownOperations: 1},
		{SeatRuntime: runtimeapi.SeatRuntime{SeatID: repID, SeatKey: "rep_a", State: "Stopped", PendingDeliveries: 0}},
	}, nil
}

func (f *fakePlatform) Seat(_ context.Context, id string) (*runtimeapi.ConsoleSeatDetail, error) {
	org, _ := f.Organization(context.Background(), orgID)
	seats, _ := f.Seats(context.Background(), orgID)
	for i, c := range org.Seats {
		if c.SeatID == id {
			fin := now.Add(-time.Hour)
			return &runtimeapi.ConsoleSeatDetail{Config: c, Status: seats[i],
				Handoff: &runtimeapi.Handoff{Objective: "ship login", Unresolved: []string{"OAuth provider"}},
				Memory:  []runtimeapi.ConsoleMemoryStore{{Key: "lead", Personal: true, Records: 4}},
				Executions: []runtimeapi.ConsoleExecution{
					{ID: runID, SeatID: id, SeatKey: c.Key, LeaseGeneration: 3, State: "running", StartedAt: now.Add(-time.Minute)},
					{ID: "66666666-6666-6666-6666-666666666666", SeatID: id, SeatKey: c.Key, LeaseGeneration: 2, State: "completed",
						StartedAt: now.Add(-2 * time.Hour), FinishedAt: &fin},
				}}, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakePlatform) Execution(_ context.Context, id string) (*runtimeapi.ConsoleExecutionDetail, error) {
	if id != runID {
		return nil, ErrNotFound
	}
	return &runtimeapi.ConsoleExecutionDetail{
		Execution: runtimeapi.ConsoleExecution{ID: runID, SeatID: leadID, SeatKey: "lead", LeaseGeneration: 3, State: "failed",
			Error: "harness exited", StartedAt: now.Add(-time.Minute)},
		Trigger: &runtimeapi.ConsoleMessage{ID: "m1", ConversationID: convID, Origin: "seat", SenderSeat: "rep_a", RecipientSeat: "lead",
			Body: "please build the login page", CreatedAt: now.Add(-time.Minute),
			Deliveries: []runtimeapi.ConsoleDelivery{{SeatKey: "lead", State: "leased", Attempts: 1, ExecutionID: runID}}},
		Events: []runtimeapi.ConsoleEvent{
			{ID: 1, Kind: "tool_request", CorrelationID: "tu1", Data: json.RawMessage(`{"name":"connections.invoke"}`), CreatedAt: now.Add(-50 * time.Second)},
			{ID: 2, Kind: "tool_result", CorrelationID: "tu1", Data: json.RawMessage(`{"content":"ok"}`), CreatedAt: now.Add(-49 * time.Second)},
			{ID: 3, Kind: "error", Data: json.RawMessage(`{"error":"<script>alert(1)</script>"}`), CreatedAt: now.Add(-40 * time.Second)},
		},
	}, nil
}

func (f *fakePlatform) Conversations(context.Context, string) ([]runtimeapi.ConsoleConversation, error) {
	return []runtimeapi.ConsoleConversation{{ID: convID, Kind: "internal", Participants: []string{"lead", "rep_a"}, Messages: 1, LastAt: now}}, nil
}

func (f *fakePlatform) Conversation(_ context.Context, id string) (*runtimeapi.ConsoleConversationDetail, error) {
	d, _ := f.Execution(context.Background(), runID)
	return &runtimeapi.ConsoleConversationDetail{Conversation: runtimeapi.ConsoleConversation{ID: id, Kind: "internal",
		Participants: []string{"lead", "rep_a"}, Messages: 1}, Messages: []runtimeapi.ConsoleMessage{*d.Trigger}}, nil
}

func (f *fakePlatform) Operations(_ context.Context, _, seat, status string) ([]runtimeapi.ConsoleOperation, error) {
	ops := []runtimeapi.ConsoleOperation{
		{Operation: runtimeapi.Operation{ID: "o1", Connection: "tracker", Operation: "task.write", Status: "succeeded", ExternalReceipt: "LIN-42"}, SeatKey: "lead", ExecutionID: runID},
		{Operation: runtimeapi.Operation{ID: "o2", Connection: "tracker", Operation: "project.create", Status: "unknown"}, SeatKey: "lead"},
	}
	var out []runtimeapi.ConsoleOperation
	for _, o := range ops {
		if (seat == "" || o.SeatKey == seat) && (status == "" || o.Status == status) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakePlatform) Artifacts(context.Context, string) ([]runtimeapi.ConsoleArtifact, error) {
	return []runtimeapi.ConsoleArtifact{{ID: "a1", OwnerSeat: "lead", Location: "workspace://design.md", ContentDigest: "sha256:ffff0000", SizeBytes: 120}}, nil
}

func (f *fakePlatform) Executions(context.Context, string, string) ([]runtimeapi.ConsoleExecution, error) {
	return []runtimeapi.ConsoleExecution{{ID: runID, SeatKey: "lead", State: "failed", Error: "harness exited", StartedAt: now.Add(-time.Minute)}}, nil
}

func (f *fakePlatform) Work(context.Context, string) ([]runtimeapi.ConsoleWorkItem, error) {
	return []runtimeapi.ConsoleWorkItem{{ID: "engineering/W-3", Store: "engineering", Objective: "Ship the login page", Creator: "lead",
		Owner: "lead", Status: "blocked", Blocker: "waiting for the OAuth client id", Evidence: []string{"PR #12"},
		Plan:         []runtimeapi.ConsoleWorkStep{{Step: "wire the form", Status: "in_progress"}},
		Publications: []runtimeapi.ConsoleWorkPublication{{Connection: "tracker", State: "blocked", LastError: "linear unreachable"}}}}, nil
}

func (f *fakePlatform) Activity(_ context.Context, _ string, after time.Time, limit int) ([]runtimeapi.ConsoleActivityItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtimeapi.ConsoleActivityItem
	for _, it := range f.activity {
		if after.IsZero() || it.At.After(after) {
			out = append(out, it)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (f *fakePlatform) add(it runtimeapi.ConsoleActivityItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity = append(f.activity, it)
}

type fakeCluster struct{}

func (fakeCluster) Organization(context.Context, string) (*v1alpha1.AgentOrganization, error) {
	o := &v1alpha1.AgentOrganization{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"}}
	o.Spec.Key = "acme"
	o.Status.OrganizationID = orgID
	o.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.CondOperationalReady, Status: metav1.ConditionFalse, Reason: "ConnectionsUnauthenticated"},
		{Type: v1alpha1.CondConnectionsAuthenticated, Status: metav1.ConditionFalse, Reason: "TrackerFailed", Message: "tracker: 401"},
	}
	return o, nil
}

func (fakeCluster) Seats(context.Context, *v1alpha1.AgentOrganization) (map[string]v1alpha1.AgentSeat, error) {
	lead := v1alpha1.AgentSeat{Spec: v1alpha1.AgentSeatSpec{SeatID: leadID, SeatKey: "lead", ConfigRevision: "rev-3",
		Workspace: v1alpha1.WorkspaceRef{ClaimName: "ws-lead-1234"}}}
	lead.Status.ExecutionState = v1alpha1.StateExecuting
	lead.Status.AdoptedRevision = "rev-2"
	lead.Status.Probe = &v1alpha1.SeatProbeStatus{Status: "passed"}
	return map[string]v1alpha1.AgentSeat{leadID: lead}, nil
}

func (fakeCluster) Pods(context.Context, *v1alpha1.AgentOrganization) (map[string]corev1.Pod, error) {
	return map[string]corev1.Pod{"lead": {ObjectMeta: metav1.ObjectMeta{Name: "seat-lead-1234-0"}}}, nil
}

func (fakeCluster) Claims(context.Context, string) (map[string]corev1.PersistentVolumeClaimPhase, error) {
	return map[string]corev1.PersistentVolumeClaimPhase{"ws-lead-1234": corev1.ClaimBound}, nil
}

func newTestServer(t *testing.T, p *fakePlatform) *httptest.Server {
	t.Helper()
	h, err := New(Config{Platform: p, Cluster: fakeCluster{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 20 * time.Millisecond, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestPagesRender(t *testing.T) {
	p := &fakePlatform{activity: []runtimeapi.ConsoleActivityItem{
		{Key: "m:1", Kind: "message", At: now.Add(-time.Minute), SeatKey: "rep_a", PeerSeat: "lead", MessageID: "m1",
			ConversationID: convID, ExecutionID: runID, Status: "seat", Summary: "please build the login page"},
	}}
	srv := newTestServer(t, p)
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/orgs/" + orgID, []string{"Engineering Lead", "working", "lease held", "please build the login page", "1 operation with unknown outcome",
			"running an older configuration revision", "Representatives", "not ready"}},
		{"/orgs/" + orgID + "/grid", []string{"Engineering Lead", "Alice&#39;s rep"}},
		{"/orgs/" + orgID + "/work", []string{"LIN-42", "Needs attention", "project.create", "workspace://design.md", "harness exited",
			"engineering/W-3", "waiting for the OAuth client id", "wire the form", "PR #12", "linear unreachable"}},
		{"/orgs/" + orgID + "/work?status=succeeded", []string{"LIN-42"}},
		{"/orgs/" + orgID + "/activity", []string{"comm-map", "edge-lead--rep_a", "node-lead", "please build the login page", "/stream?after="}},
		{"/orgs/" + orgID + "/readiness", []string{"ConnectionsAuthenticated", "older", "ws-lead-1234", "Bound", "steadmesh/linear", "401 from Linear", "passed", "replacement_rejected", "secret version 4"}},
		{"/seats/" + leadID, []string{leadID, "ship login", "OAuth provider", "<code>lead</code> (personal)", "seat-lead-1234-0", "connection:tracker"}},
		{"/runs/" + runID, []string{"harness exited", "tool call", "result after 1.0s", "connections.invoke", "please build the login page"}},
		{"/conversations/" + convID + "?org=" + orgID, []string{"please build the login page", "leased"}},
	} {
		code, body := get(t, srv, tc.path)
		if code != http.StatusOK {
			t.Errorf("%s: %d %s", tc.path, code, body)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", tc.path, w)
			}
		}
	}
	// Filters apply.
	if _, body := get(t, srv, "/orgs/"+orgID+"/work?status=succeeded"); strings.Contains(body, "project.create") {
		t.Error("status filter not applied")
	}
	// Event data is escaped, never interpreted.
	if _, body := get(t, srv, "/runs/"+runID); strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("event data rendered unescaped")
	}
	if code, _ := get(t, srv, "/runs/nope"); code != http.StatusNotFound {
		t.Errorf("unknown run: %d", code)
	}
	if code, _ := get(t, srv, "/"); code != http.StatusFound {
		t.Errorf("single organisation index should redirect: %d", code)
	}
	if code, body := get(t, srv, "/static/console.css"); code != http.StatusOK || !strings.Contains(body, "--accent") {
		t.Errorf("static: %d", code)
	}
}

func TestStreamSendsNewActivityOnce(t *testing.T) {
	p := &fakePlatform{}
	srv := newTestServer(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/orgs/"+orgID+"/stream?after="+now.Add(-time.Hour).Format(time.RFC3339Nano), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	item := runtimeapi.ConsoleActivityItem{Key: "m:9", Kind: "message", At: now.Add(-time.Minute), SeatKey: "rep_a", PeerSeat: "lead",
		ConversationID: convID, Summary: "hand <b>off</b>"}
	p.add(item)

	sc := bufio.NewScanner(res.Body)
	var got []streamEvent
	deadline := time.After(3 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	polls := 0
	for polls < 5 {
		select {
		case <-deadline:
			t.Fatalf("timed out; got %+v", got)
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			if l == ": keepalive" {
				polls++
			}
			if d, ok := strings.CutPrefix(l, "data: "); ok {
				var ev streamEvent
				if err := json.Unmarshal([]byte(d), &ev); err != nil {
					t.Fatal(err)
				}
				got = append(got, ev)
			}
		}
	}
	// The item stays inside the overlap window for every poll, but is sent once.
	if len(got) != 1 || got[0].Seat != "rep_a" || got[0].Peer != "lead" || got[0].Kind != "message" {
		t.Fatalf("events = %+v", got)
	}
	if !strings.Contains(got[0].HTML, "hand &lt;b&gt;off&lt;/b&gt;") {
		t.Fatalf("fragment not escaped: %s", got[0].HTML)
	}
}

func TestSeatState(t *testing.T) {
	live := runtimeapi.SeatRuntime{SeatID: "s", LeaseHolder: "p", LeaseExpiresAt: now.Add(time.Minute)}
	crd := func(state v1alpha1.ExecutionState) *v1alpha1.AgentSeat {
		s := &v1alpha1.AgentSeat{}
		s.Status.ExecutionState = state
		return s
	}
	queued := live
	queued.PendingDeliveries = 2
	for _, tc := range []struct {
		name string
		st   runtimeapi.SeatRuntime
		crd  *v1alpha1.AgentSeat
		want string
	}{
		{"no record", runtimeapi.SeatRuntime{}, nil, StateUnknown},
		{"idle and warm", live, crd(v1alpha1.StateWarm), StateWaiting},
		{"warm with inbox", queued, crd(v1alpha1.StateWarm), StateQueued},
		{"executing", live, crd(v1alpha1.StateExecuting), StateWorking},
		{"blocked", live, crd(v1alpha1.StateBlocked), StateBlocked},
		{"recovering", live, crd(v1alpha1.StateRecovering), StateStarting},
		{"stopped", runtimeapi.SeatRuntime{SeatID: "s", State: "Stopped"}, nil, StateOffline},
		{"stopped with inbox", runtimeapi.SeatRuntime{SeatID: "s", State: "Stopped", PendingDeliveries: 1}, nil, StateQueued},
	} {
		v := newSeatView(runtimeapi.ConsoleSeatConfig{}, runtimeapi.ConsoleSeatStatus{SeatRuntime: tc.st}, tc.crd, nil, now)
		if v.State != tc.want {
			t.Errorf("%s: state %s, want %s", tc.name, v.State, tc.want)
		}
	}
}

func TestPairEvents(t *testing.T) {
	steps := pairEvents([]runtimeapi.ConsoleEvent{
		{ID: 1, Kind: "tool_request", CorrelationID: "a"},
		{ID: 2, Kind: "tool_request", CorrelationID: "b"},
		{ID: 3, Kind: "tool_result", CorrelationID: "b"},
		{ID: 4, Kind: "tool_result", CorrelationID: "a"},
		{ID: 5, Kind: "tool_result", CorrelationID: "orphan"},
		{ID: 6, Kind: "output"},
	})
	if len(steps) != 4 || steps[0].Result.ID != 4 || steps[1].Result.ID != 3 || steps[2].Event.ID != 5 || steps[3].Event.ID != 6 {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestStreamSkipsItemsAlreadyOnThePage(t *testing.T) {
	onPage := runtimeapi.ConsoleActivityItem{Key: "m:1", Kind: "message", At: now.Add(-2 * time.Second), SeatKey: "rep_a", Summary: "old"}
	p := &fakePlatform{activity: []runtimeapi.ConsoleActivityItem{onPage}}
	srv := newTestServer(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/orgs/"+orgID+"/stream?after="+onPage.At.Format(time.RFC3339Nano), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	p.add(runtimeapi.ConsoleActivityItem{Key: "m:2", Kind: "message", At: now.Add(-time.Second), SeatKey: "rep_a", Summary: "new"})
	sc := bufio.NewScanner(res.Body)
	var data []string
	for keepalives := 0; keepalives < 3 && sc.Scan(); {
		l := sc.Text()
		if l == ": keepalive" {
			keepalives++
		}
		if d, ok := strings.CutPrefix(l, "data: "); ok {
			data = append(data, d)
		}
	}
	if len(data) != 1 || !strings.Contains(data[0], "new") {
		t.Fatalf("stream sent %v, want only the new item", data)
	}
}

func TestShortRef(t *testing.T) {
	ref := "configmap:steadmesh-role-engineer/engineer.md#sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := shortRef(ref); got != "configmap:steadmesh-role-engineer/engineer.md#sha256:0123456789ab…" {
		t.Fatalf("shortRef = %q", got)
	}
	if got := shortRef("role:engineer"); got != "role:engineer" {
		t.Fatalf("shortRef = %q", got)
	}
}
