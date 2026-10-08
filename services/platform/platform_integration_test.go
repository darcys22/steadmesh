//go:build integration

package platform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/platform"
	"github.com/darcys22/steadmesh/services/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const (
	controllerToken = "controller-token"
	consoleToken    = "console-token"
)

type env struct {
	t       *testing.T
	srv     *httptest.Server
	auth    *auth.Fake
	comm    *fakeconn.Comm
	tracker *fakeconn.Tracker
	org     string
	seats   map[string]string
}

func newEnv(t *testing.T) *env { return newEnvWithConsole(t, false) }

// newEnvWithConsole starts a platform; with console it serves /console/v1 to
// consoleToken.
func newEnvWithConsole(t *testing.T, console bool) *env { return newEnvWith(t, console, nil) }

// newEnvWith starts a platform after applying opts to its options.
func newEnvWith(t *testing.T, console bool, opts func(*platform.Options)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, auth: auth.NewFake(controllerToken), comm: fakeconn.NewComm(orgfixture.UserA, orgfixture.UserB),
		tracker: &fakeconn.Tracker{}}
	if console {
		e.auth.SetConsole(consoleToken)
	}
	o := platform.Options{
		Store: st, Auth: e.auth, Console: console, Secrets: &fakeconn.Secrets{}, Registry: prometheus.NewRegistry(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: 50 * time.Millisecond, RetryBackoff: time.Millisecond,
		Factories: connections.Factories{
			Communication: map[string]func(connectors.Config) (connectors.Communication, error){"slack": e.comm.Factory()},
			Tracker: map[string]func(connectors.Config) (connectors.Tracker, error){
				"linear": func(connectors.Config) (connectors.Tracker, error) { return e.tracker, nil }},
			Model: map[string]func(connectors.Config) (connectors.Model, error){
				"anthropic": func(connectors.Config) (connectors.Model, error) { return fakeconn.Model{}, nil }},
		},
	}
	if opts != nil {
		opts(&o)
	}
	p := platform.New(ctx, o)
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(p.Handler)
	t.Cleanup(func() {
		e.srv.Close()
		cancel()
		p.Wait()
		st.Close()
	})
	return e
}

func (e *env) request(method, path, token string, hdr map[string]string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *env) sync(s spec.OrganizationSpec) runtimeapi.SyncResponse {
	e.t.Helper()
	raw, _ := json.Marshal(orgfixture.Compile(e.t, s))
	code, b := e.request("POST", runtimeapi.PathInternalSync, controllerToken, nil,
		runtimeapi.SyncRequest{Namespace: "acme", Key: "acme", SourceUID: "uid", Manifest: raw})
	if code != http.StatusOK {
		e.t.Fatalf("sync: %d %s", code, b)
	}
	var res runtimeapi.SyncResponse
	_ = json.Unmarshal(b, &res)
	e.org, e.seats = res.OrganizationID, map[string]string{}
	for k, id := range res.Seats {
		e.seats[k] = id.SeatID
		e.auth.AddSeat("tok-"+k, auth.Seat{SeatID: id.SeatID, PodUID: "pod-" + k})
	}
	return res
}

// seat is a seat client holding the lease.
type seat struct {
	e     *env
	key   string
	token string
	gen   int64
}

func (e *env) seat(key string) *seat {
	e.t.Helper()
	s := &seat{e: e, key: key, token: "tok-" + key}
	code, b := e.request("POST", runtimeapi.PathLeaseAcquire, s.token, nil, runtimeapi.LeaseAcquireRequest{})
	if code != http.StatusOK {
		e.t.Fatalf("acquire %s: %d %s", key, code, b)
	}
	var l runtimeapi.LeaseResponse
	_ = json.Unmarshal(b, &l)
	s.gen = l.Generation
	return s
}

func (s *seat) do(method, path string, body any) (int, []byte) {
	return s.e.request(method, path, s.token, map[string]string{runtimeapi.HeaderGeneration: fmt.Sprint(s.gen)}, body)
}

// tool calls a tool and returns its decoded content and error flag.
func (s *seat) tool(name string, args any) (map[string]any, bool) {
	s.e.t.Helper()
	raw, _ := json.Marshal(args)
	code, b := s.do("POST", runtimeapi.PathToolCall+name, runtimeapi.ToolCallRequest{Arguments: raw})
	if code != http.StatusOK {
		s.e.t.Fatalf("%s %s: %d %s", s.key, name, code, b)
	}
	var res runtimeapi.ToolCallResult
	_ = json.Unmarshal(b, &res)
	var out map[string]any
	_ = json.Unmarshal(res.Content, &out)
	return out, res.IsError
}

func (s *seat) mustTool(name string, args any) map[string]any {
	s.e.t.Helper()
	out, isErr := s.tool(name, args)
	if isErr {
		s.e.t.Fatalf("%s %s failed: %v", s.key, name, out)
	}
	return out
}

func (s *seat) next() *runtimeapi.InboxDelivery {
	s.e.t.Helper()
	code, b := s.do("GET", runtimeapi.PathInboxNext+"?wait=2", nil)
	switch code {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
		var d runtimeapi.InboxDelivery
		_ = json.Unmarshal(b, &d)
		return &d
	}
	s.e.t.Fatalf("inbox next: %d %s", code, b)
	return nil
}

func (s *seat) ack(d *runtimeapi.InboxDelivery) {
	s.e.t.Helper()
	code, b := s.do("POST", fmt.Sprintf("%s%d/ack", runtimeapi.PathInboxAck, d.DeliveryID),
		runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID, Outcome: "completed"})
	if code != http.StatusNoContent {
		s.e.t.Fatalf("ack: %d %s", code, b)
	}
}

func toolNames(s *seat) []string {
	_, b := s.do("GET", runtimeapi.PathTools, nil)
	var ds []runtimeapi.ToolDescriptor
	_ = json.Unmarshal(b, &ds)
	var out []string
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

func TestAuthenticationBoundaries(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	if code, _ := e.request("GET", runtimeapi.PathSelf, "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous self: %d", code)
	}
	// A seat token cannot reach the management API (A17).
	if code, _ := e.request("GET", runtimeapi.PathInternalOrgs+e.org+"/runtime", "tok-lead", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("seat on internal API: %d", code)
	}
	if code, _ := e.request("GET", "/healthz", "", nil, nil); code != http.StatusOK {
		t.Fatal("healthz")
	}
	if code, _ := e.request("GET", "/readyz", "", nil, nil); code != http.StatusOK {
		t.Fatal("readyz")
	}
	if code, b := e.request("GET", "/metrics", "", nil, nil); code != http.StatusOK || !strings.Contains(string(b), "steadmesh_http_requests_total") {
		t.Fatalf("metrics: %d", code)
	}
}

func TestFencingRejectsStaleGeneration(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	lead := e.seat("lead")
	args := map[string]any{"store": "lead", "path": "notes/t.md", "text": "b"}
	raw, _ := json.Marshal(args)
	// Missing generation is stale.
	code, b := e.request("POST", runtimeapi.PathToolCall+"memory.write", lead.token, nil, runtimeapi.ToolCallRequest{Arguments: raw})
	if code != http.StatusConflict || !strings.Contains(string(b), `"fenced"`) {
		t.Fatalf("missing generation: %d %s", code, b)
	}
	// The controller fences after confirming the old Pod is gone.
	code, b = e.request("POST", runtimeapi.PathInternalSeats+e.seats["lead"]+"/fence", controllerToken, nil,
		runtimeapi.FenceRequest{ExpectedGeneration: lead.gen})
	if code != http.StatusOK {
		t.Fatalf("fence: %d %s", code, b)
	}
	for _, call := range []struct{ method, path string }{
		{"POST", runtimeapi.PathToolCall + "memory.write"},
		{"GET", runtimeapi.PathInboxNext},
		{"PUT", runtimeapi.PathCheckpoint},
	} {
		var body any = runtimeapi.ToolCallRequest{Arguments: raw}
		if call.method == "PUT" {
			body = runtimeapi.Checkpoint{HarnessAdapter: "fake", CheckpointRef: "s1"}
		}
		if code, b := lead.do(call.method, call.path, body); code != http.StatusConflict || !strings.Contains(string(b), `"fenced"`) {
			t.Errorf("%s %s with stale generation: %d %s", call.method, call.path, code, b)
		}
	}
	if code, _ := lead.do("POST", runtimeapi.PathLeaseRenew, runtimeapi.LeaseRenewRequest{Generation: lead.gen}); code != http.StatusConflict {
		t.Fatalf("stale renew: %d", code)
	}
	lead = e.seat("lead")
	lead.mustTool("memory.write", args)
}

func TestRoutesAndReplySemantics(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	repA, lead, eng, reviewer := e.seat("rep_a"), e.seat("lead"), e.seat("engineer"), e.seat("reviewer")

	if out, isErr := repA.tool("messages.send", map[string]any{"to": "engineer", "body": "skip the lead"}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("undeclared route allowed: %v", out)
	}
	if slices.Contains(toolNames(reviewer), "messages.send") {
		t.Fatal("reviewer without routes is offered messages.send")
	}
	sent := repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "please build the login page"})
	d := lead.next()
	if d == nil || d.Message.SenderSeat != "rep_a" || d.Message.Origin != "seat" || d.Message.CorrelationID != sent["correlation_id"] {
		t.Fatalf("delivery = %+v", d)
	}
	lead.ack(d)
	// The route has reply=true, so the lead may reply although it has no route to rep_a.
	reply := lead.mustTool("messages.reply", map[string]any{"message_id": d.Message.MessageID, "body": "on it"})
	if reply["correlation_id"] != sent["correlation_id"] || reply["conversation_id"] != sent["conversation_id"] {
		t.Fatalf("reply not correlated: %v vs %v", reply, sent)
	}
	got := repA.next()
	if got == nil || got.Message.ParentID != d.Message.MessageID || got.Message.SenderSeat != "lead" {
		t.Fatalf("reply delivery = %+v", got)
	}
	repA.ack(got)

	// engineer -> reviewer has no reply semantics.
	eng.mustTool("messages.send", map[string]any{"to": "reviewer", "body": "review PR 7"})
	rd := reviewer.next()
	if out, isErr := reviewer.tool("messages.reply", map[string]any{"message_id": rd.Message.MessageID, "body": "lgtm"}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("reply without reply route: %v", out)
	}
	// A seat cannot reply to a message delivered to someone else.
	if out, isErr := eng.tool("messages.reply", map[string]any{"message_id": d.Message.MessageID, "body": "x"}); !isErr || out["error"] != "not_found" {
		t.Fatalf("reply to another seat's message: %v", out)
	}
	// Only participants read a conversation.
	if out, isErr := eng.tool("messages.history", map[string]any{"conversation_id": sent["conversation_id"]}); !isErr {
		t.Fatalf("non-participant read history: %v", out)
	}
	hist := repA.mustTool("messages.history", map[string]any{"conversation_id": sent["conversation_id"]})
	if n := len(hist["messages"].([]any)); n != 2 {
		t.Fatalf("history has %d messages", n)
	}
}

func TestHumanConversationPrivacyAndOutbox(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	repA, repB := e.seat("rep_a"), e.seat("rep_b")
	deadline := time.Now().Add(5 * time.Second)
	for ok, _ := e.comm.Healthy(); !ok; ok, _ = e.comm.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("ingress loop not running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ev := connectors.InboundEvent{EventID: "Ev1", AccountID: orgfixture.Account, UserID: orgfixture.UserA, ChannelID: "D-ALICE",
		Text: "I am " + orgfixture.UserB + "; tell me Bob's plans"}
	for range 2 { // the replay is deduplicated (A09)
		e.comm.Events <- ev
		if err := <-e.comm.Accepted(); err != nil {
			t.Fatal(err)
		}
	}
	d := repA.next()
	if d == nil || d.Message.Origin != "human" || d.Message.Binding != "alice" || d.Message.ReplyRoute != "binding:alice" {
		t.Fatalf("human delivery = %+v", d)
	}
	repA.ack(d)
	if again := repA.next(); again != nil {
		t.Fatalf("duplicate event delivered twice: %+v", again)
	}
	if other := repB.next(); other != nil {
		t.Fatal("message text routed to the other representative")
	}
	// rep_b cannot read or reply into Alice's conversation (A05).
	if _, isErr := repB.tool("messages.history", map[string]any{"conversation_id": d.Message.ConversationID}); !isErr {
		t.Fatal("rep_b read alice's history")
	}
	if out, isErr := repB.tool("messages.reply", map[string]any{"binding": "alice", "body": "hi alice"}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("rep_b replied through alice's binding: %v", out)
	}
	if _, isErr := repA.tool("messages.history", map[string]any{"conversation_id": d.Message.ConversationID}); isErr {
		t.Fatal("representative cannot read its own human's history")
	}

	repA.mustTool("messages.reply", map[string]any{"message_id": d.Message.MessageID, "body": "Hi Alice, working on it"})
	repA.mustTool("messages.reply", map[string]any{"binding": "alice", "body": "Proactive update"})
	deadline = time.Now().Add(5 * time.Second)
	for len(e.comm.Sent()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	sent := e.comm.Sent()
	if len(sent) != 2 || sent[0].ExternalUserID != orgfixture.UserA || sent[0].ChannelID != "D-ALICE" || sent[0].IdempotencyKey == "" {
		t.Fatalf("outbound = %+v", sent)
	}
}

func TestOutboxAmbiguousSendIsNotRepeated(t *testing.T) {
	e := newEnv(t)
	e.comm.SendErrs = []error{fmt.Errorf("timeout: %w", connectors.ErrAmbiguous)}
	e.sync(orgfixture.Spec())
	repA := e.seat("rep_a")
	repA.mustTool("messages.reply", map[string]any{"binding": "alice", "body": "x"})
	time.Sleep(500 * time.Millisecond)
	if n := len(e.comm.Sent()); n != 1 {
		t.Fatalf("ambiguous send attempted %d times", n)
	}
	_, b := repA.do("GET", runtimeapi.PathBootstrap, nil)
	var boot runtimeapi.Bootstrap
	_ = json.Unmarshal(b, &boot)
	if len(boot.Recovery.UnknownOperations) != 1 || boot.Recovery.UnknownOperations[0].Operation != "channel.reply" {
		t.Fatalf("recovery = %+v", boot.Recovery)
	}
}

func TestGrantRevocationIsImmediate(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	eng := e.seat("engineer")
	rec := eng.mustTool("memory.write", map[string]any{"store": "engineering", "path": "notes/runbook.md", "text": "deploy steps"})
	if !slices.Contains(toolNames(eng), "memory.append") {
		t.Fatal("memory.append not offered")
	}

	sp := orgfixture.Spec()
	sp.Seats["engineer"] = func(s spec.Seat) spec.Seat { s.Teams = nil; return s }(sp.Seats["engineer"])
	res := e.sync(sp)
	if res.Seats["engineer"].PolicyRevision != 2 {
		t.Fatalf("policy revision = %d", res.Seats["engineer"].PolicyRevision)
	}
	// The very next call, on the same lease and session, is denied.
	if out, isErr := eng.tool("memory.read", map[string]any{"record_id": rec["record_id"]}); !isErr || out["error"] != "not_found" {
		t.Fatalf("read after revocation: %v", out)
	}
	if out, isErr := eng.tool("memory.write", map[string]any{"store": "engineering", "path": "notes/x.md", "text": "y"}); !isErr {
		t.Fatalf("write after revocation: %v", out)
	}
	if hits := eng.mustTool("memory.search", map[string]any{"query": "deploy"}); len(hits["results"].([]any)) != 0 {
		t.Fatalf("search after revocation: %v", hits)
	}
	if out, _ := eng.tool("self", nil); out["policy_revision"].(float64) != 2 {
		t.Fatalf("self = %v", out)
	}
}

func TestMemoryToolsConflictAndNoLeak(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	lead, eng, repA := e.seat("lead"), e.seat("engineer"), e.seat("rep_a")
	rec := lead.mustTool("memory.write", map[string]any{"store": "engineering", "path": "notes/api-design.md", "text": "v1", "tags": []string{"api"}})
	id := rec["record_id"]
	lead.mustTool("memory.write", map[string]any{"store": "engineering", "path": "notes/api-design.md", "text": "v2 lead", "expected_revision": 1})
	out, isErr := eng.tool("memory.write", map[string]any{"store": "engineering", "path": "notes/api-design.md", "text": "v2 engineer", "expected_revision": 1})
	if !isErr || out["error"] != "conflict" || out["details"].(map[string]any)["current_revision"].(float64) != 2 {
		t.Fatalf("stale revise: %v", out)
	}
	hist := eng.mustTool("memory.history", map[string]any{"record_id": id})
	if revs := hist["revisions"].([]any); len(revs) != 2 || revs[0].(map[string]any)["author_seat"] != "lead" {
		t.Fatalf("history = %v", hist)
	}

	// rep_a can search organisation and rep_a only; engineering content must not leak.
	lead.mustTool("memory.write", map[string]any{"store": "organisation", "path": "notes/company-holidays.md", "text": "API freeze in December"})
	hits := repA.mustTool("memory.search", map[string]any{"query": "API"})
	results := hits["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["store"] != "organisation" {
		t.Fatalf("search leaked: %v", hits)
	}
	if out, isErr := repA.tool("memory.search", map[string]any{"query": "API", "stores": []string{"engineering"}}); !isErr || out["error"] != "not_found" {
		t.Fatalf("explicit inaccessible store: %v", out)
	}
	if out, isErr := repA.tool("memory.read", map[string]any{"record_id": id}); !isErr || out["error"] != "not_found" {
		t.Fatalf("read inaccessible record: %v", out)
	}
	// Publish copies with provenance into an authorised destination.
	mine := repA.mustTool("memory.write", map[string]any{"store": "rep_a", "path": "notes/alice-prefers-short-updates.md", "text": "weekly"})
	if out, isErr := repA.tool("memory.publish", map[string]any{"record_id": mine["record_id"], "destination": "organisation"}); !isErr {
		t.Fatalf("publish without write on destination: %v", out)
	}
	pub := lead.mustTool("memory.write", map[string]any{"store": "lead", "path": "notes/team-norms.md", "text": "review within a day"})
	cp := lead.mustTool("memory.publish", map[string]any{"record_id": pub["record_id"], "destination": "organisation"})
	if refs := cp["source_refs"].([]any); len(refs) != 1 || !strings.HasPrefix(refs[0].(string), "memory:"+pub["record_id"].(string)) {
		t.Fatalf("provenance = %v", cp)
	}
}

func TestConnectionsInvokeLedger(t *testing.T) {
	e := newEnv(t)
	e.tracker.Script = []error{fmt.Errorf("response lost: %w", connectors.ErrAmbiguous)}
	e.sync(orgfixture.Spec())
	lead, eng := e.seat("lead"), e.seat("engineer")
	if slices.Contains(toolNames(eng), "connections.invoke") {
		t.Fatal("engineer without grant is offered connections.invoke")
	}
	args := map[string]any{"connection": "tracker", "operation": "project.create", "params": map[string]any{"name": "Login"}}
	op := lead.mustTool("connections.invoke", args)
	if op["status"] != "unknown" {
		t.Fatalf("op = %v", op)
	}
	again := lead.mustTool("connections.invoke", args)
	if again["id"] != op["id"] || len(e.tracker.Calls()) != 1 {
		t.Fatalf("unknown operation replayed: %v calls=%d", again, len(e.tracker.Calls()))
	}
	got := lead.mustTool("operations.get", map[string]any{"id": op["id"]})
	if got["status"] != "unknown" {
		t.Fatalf("operations.get = %v", got)
	}
	ok := lead.mustTool("connections.invoke", map[string]any{"connection": "tracker", "operation": "task.write",
		"params": map[string]any{"title": "write tests"}, "idempotency_key": "task-1"})
	if ok["status"] != "succeeded" || ok["external_receipt"] == "" {
		t.Fatalf("op = %v", ok)
	}
	if out, isErr := lead.tool("connections.invoke", map[string]any{"connection": "tracker", "operation": "comment.write"}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("ungranted operation: %v", out)
	}
}

func TestModelProxy(t *testing.T) {
	e := newEnv(t)
	sp := orgfixture.Spec()
	sp.Connections["model"] = spec.Connection{Adapter: "anthropic", SecretRef: "k8s:anthropic"}
	sp.HarnessProfiles["claude"] = spec.HarnessProfile{Adapter: "claude-code", ImageDigest: "seat:dev", Model: &spec.ModelSelection{Connection: "model", ID: "claude-x"}}
	sp.Seats["lead"] = func(s spec.Seat) spec.Seat { s.HarnessProfile = "claude"; return s }(sp.Seats["lead"])
	e.sync(sp)
	code, b := e.request("POST", "/v1/model/model/v1/messages", "", map[string]string{"X-Api-Key": "tok-lead"}, map[string]any{"model": "claude-x"})
	var echo map[string]string
	_ = json.Unmarshal(b, &echo)
	if code != http.StatusOK || echo["path"] != "/v1/messages" || echo["x_api_key"] != "" || echo["authorization"] != "" {
		t.Fatalf("proxy: %d %s", code, b)
	}
	if code, _ := e.request("GET", "/v1/model/model/v1/models", "tok-engineer", nil, nil); code != http.StatusForbidden {
		t.Fatalf("seat without model.infer: %d", code)
	}
	// Only the profile's model, over the profile's API, through model API paths.
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/v1/model/model/v1/messages", map[string]any{"model": "claude-other"}, http.StatusForbidden},
		{"POST", "/v1/model/model/v1/messages", map[string]any{}, http.StatusForbidden},
		{"POST", "/v1/model/model/v1/chat/completions", map[string]any{"model": "claude-x"}, http.StatusForbidden},
		{"POST", "/v1/model/model/v1/files", map[string]any{"model": "claude-x"}, http.StatusNotFound},
		{"DELETE", "/v1/model/model/v1/messages", nil, http.StatusMethodNotAllowed},
		{"GET", "/v1/model/model/v1/models", nil, http.StatusOK},
	} {
		if code, b := e.request(c.method, c.path, "tok-lead", nil, c.body); code != c.want {
			t.Errorf("%s %s %v: %d %s, want %d", c.method, c.path, c.body, code, b, c.want)
		}
	}
}

func TestVerifyProbeRuntimeAndBootstrap(t *testing.T) {
	e := newEnv(t)
	sp := orgfixture.Spec()
	// carol's user id does not exist in the workspace, so her binding is invalid.
	carol := sp.Seats["rep_b"]
	carol.PersonalMemory = ""
	sp.Seats["rep_c"] = carol
	sp.ChannelBindings["carol"] = spec.ChannelBinding{Connection: "slack", ExternalUserID: "U0NOBODY", Seat: "rep_c"}
	e.sync(sp)
	code, b := e.request("POST", runtimeapi.PathInternalOrgs+e.org+"/verify", controllerToken, nil, nil)
	var v runtimeapi.VerifyResponse
	_ = json.Unmarshal(b, &v)
	if code != http.StatusOK || !v.Connections["slack"].OK || !v.Connections["tracker"].OK || !v.Bindings["alice"].OK || v.Bindings["carol"].OK {
		t.Fatalf("verify: %d %s", code, b)
	}

	code, b = e.request("POST", runtimeapi.PathInternalSeats+e.seats["engineer"]+"/probe", controllerToken, nil, nil)
	var p runtimeapi.ProbeResponse
	_ = json.Unmarshal(b, &p)
	if code != http.StatusAccepted || p.Status != "pending" {
		t.Fatalf("probe: %d %s", code, b)
	}
	code, b = e.request("GET", runtimeapi.PathInternalOrgs+e.org+"/runtime", controllerToken, nil, nil)
	var rt runtimeapi.RuntimeResponse
	_ = json.Unmarshal(b, &rt)
	if code != http.StatusOK || rt.Seats["engineer"].PendingDeliveries != 1 {
		t.Fatalf("runtime: %d %s", code, b)
	}
	eng := e.seat("engineer")
	d := eng.next()
	if d == nil || d.Message.Origin != "probe" {
		t.Fatalf("probe delivery = %+v", d)
	}
	code, b = eng.do("POST", runtimeapi.PathExecEvents+d.ExecutionID+"/events", runtimeapi.ExecutionEvent{
		Kind: "completion", Data: json.RawMessage(`{"checks":{"tool":"ok","workspace":"ok"}}`)})
	if code != http.StatusNoContent {
		t.Fatalf("events: %d %s", code, b)
	}
	eng.ack(d)
	_, b = e.request("GET", runtimeapi.PathInternalSeats+e.seats["engineer"]+"/probe/"+p.ProbeID, controllerToken, nil, nil)
	_ = json.Unmarshal(b, &p)
	if p.Status != "passed" || p.Checks["tool"] != "ok" {
		t.Fatalf("probe result = %s", b)
	}

	if code, _ := eng.do("PUT", runtimeapi.PathCheckpoint, runtimeapi.Checkpoint{HarnessAdapter: "fake", FormatVersion: "1", CheckpointRef: "sess-1"}); code != http.StatusNoContent {
		t.Fatalf("checkpoint: %d", code)
	}
	eng.mustTool("handoff.update", map[string]any{"objective": "ship login"})
	_, b = eng.do("GET", runtimeapi.PathBootstrap, nil)
	var boot runtimeapi.Bootstrap
	_ = json.Unmarshal(b, &boot)
	if boot.Self.SeatKey != "engineer" || boot.Recovery.Session == nil || boot.Recovery.Session.CheckpointRef != "sess-1" ||
		boot.Recovery.Handoff == nil || boot.Recovery.Handoff.Objective != "ship login" || !strings.Contains(boot.Instructions, "[team:engineering]") {
		t.Fatalf("bootstrap = %s", b)
	}
}

func TestWakeTools(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	eng := e.seat("engineer")
	at := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	sc := eng.mustTool("wake.schedule", map[string]any{"at": at, "note": "check CI"})
	deadline := time.Now().Add(5 * time.Second)
	var d *runtimeapi.InboxDelivery
	for d == nil && time.Now().Before(deadline) {
		d = eng.next()
	}
	if d == nil || d.Message.Origin != "schedule" || !strings.Contains(d.Message.Body, "check CI") {
		t.Fatalf("wake delivery = %+v", d)
	}
	if list := eng.mustTool("wake.list", nil); len(list["schedules"].([]any)) != 0 {
		t.Fatalf("one-off schedule still active: %v", list)
	}
	rec := eng.mustTool("wake.schedule", map[string]any{"every": "1h"})
	eng.mustTool("wake.cancel", map[string]any{"id": rec["id"]})
	if out, isErr := eng.tool("wake.cancel", map[string]any{"id": sc["id"]}); !isErr || out["error"] != "not_found" {
		t.Fatalf("cancel fired schedule: %v", out)
	}
}

// removeEngineer is the fixture without the engineer seat, its routes and
// its personal store.
func removeEngineer() spec.OrganizationSpec {
	sp := orgfixture.Spec()
	delete(sp.Seats, "engineer")
	delete(sp.MemoryStores, "engineer")
	delete(sp.MessageRoutes, "lead_engineer")
	delete(sp.MessageRoutes, "engineer_to_reviewer")
	return sp
}

// waitRetired waits until the platform has retired the seat: its token stops
// authenticating.
func waitRetired(t *testing.T, s *seat) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, _ := s.do("GET", runtimeapi.PathSelf, nil)
		if code == http.StatusForbidden {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not retired: self %d", s.key, code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// nextFrom waits for the seat's next delivery and acks it.
func (s *seat) nextAcked() *runtimeapi.InboxDelivery {
	s.e.t.Helper()
	d := s.next()
	if d == nil {
		s.e.t.Fatalf("%s: no delivery", s.key)
	}
	s.ack(d)
	return d
}

// TestGracefulRetirement removes a busy seat: its running turn finishes, it
// gets a retirement notice turn in which it still has its tools and personal
// memory, its queued messages go back to their senders, new messages are
// refused, and once the notice turn is done it is retired: its work item is
// released, the representatives get a summary with its handoff, and its
// access ends.
func TestGracefulRetirement(t *testing.T) {
	e := newEnvWith(t, true, func(o *platform.Options) { o.RetirementGrace = time.Minute })
	e.sync(orgfixture.Spec())
	engID := e.seats["engineer"]
	lead, eng := e.seat("lead"), e.seat("engineer")
	w := lead.mustTool("work.create", map[string]any{"store": "engineering", "objective": "Migrate the database", "owner": "engineer"})
	workID := w["work_id"].(string)

	lead.mustTool("messages.send", map[string]any{"to": "engineer", "body": "start the migration"})
	busy := eng.next() // the engineer is mid-turn
	if busy == nil {
		t.Fatal("engineer got no work")
	}
	lead.mustTool("messages.send", map[string]any{"to": "engineer", "body": "also rotate the logs"})
	lead.mustTool("messages.send", map[string]any{"to": "engineer", "body": "fyi: freeze on friday", "wake": false})

	res := e.sync(removeEngineer())
	rs, ok := res.Retiring["engineer"]
	if !ok || rs.SeatID != engID || time.Until(rs.RetireBy) < 50*time.Second {
		t.Fatalf("retiring = %+v", res.Retiring)
	}
	if again := e.sync(removeEngineer()); !again.Retiring["engineer"].RetireBy.Equal(rs.RetireBy) {
		t.Fatalf("a repeated sync moved the deadline: %+v", again.Retiring)
	}

	// Queued messages went back to the lead, quoting them.
	for _, want := range []string{"also rotate the logs", "fyi: freeze on friday"} {
		d := lead.nextAcked()
		if d.Message.Origin != "system" || !strings.Contains(d.Message.Body, "Not delivered: seat engineer is retiring") ||
			!strings.Contains(d.Message.Body, want) {
			t.Fatalf("returned message = %+v", d.Message)
		}
	}
	// The status tool and self report the wind-down.
	if self := eng.mustTool("self", nil); self["retiring_until"] == nil {
		t.Fatalf("self = %v", self)
	}
	// Retiring seats take no probes.
	if code, _ := e.request("POST", runtimeapi.PathInternalSeats+engID+"/probe", controllerToken, nil, struct{}{}); code == http.StatusOK {
		t.Fatal("a retiring seat was probed")
	}

	// The running turn finishes normally; then the retirement notice.
	eng.ack(busy)
	notice := eng.next()
	if notice == nil || notice.Message.Origin != "system" || !strings.Contains(notice.Message.Body, "Retirement notice") ||
		!strings.Contains(notice.Message.Body, "handoff.update") || len(notice.Passive) != 0 {
		t.Fatalf("notice = %+v", notice)
	}
	if !strings.Contains(notice.Message.Body, "also rotate the logs") {
		t.Fatalf("notice does not list the returned messages: %s", notice.Message.Body)
	}
	// It still has its personal memory and tools during the wind-down.
	eng.mustTool("memory.write", map[string]any{"store": "engineer", "path": "notes/migration.md", "text": "schema v2 applied to staging"})
	eng.mustTool("handoff.update", map[string]any{"objective": "Migrate the database to schema v2",
		"unresolved": []string{"production cut-over not scheduled"}, "notes": "staging done; see notes/migration.md"})
	eng.ack(notice)

	waitRetired(t, eng)
	got := lead.mustTool("work.get", map[string]any{"work_id": workID})
	if got["owner"] != "" || got["status"] != "ready" || !strings.Contains(fmt.Sprint(got["log"]), "owner engineer retired") {
		t.Fatalf("work item after retirement = %v", got)
	}
	for _, key := range []string{"rep_a", "rep_b"} {
		d := e.seat(key).nextAcked()
		b := d.Message.Body
		for _, want := range []string{"Seat engineer has retired", "finished its retirement turn", "schema v2", "production cut-over",
			workID, "Migrate the database"} {
			if !strings.Contains(b, want) {
				t.Fatalf("%s summary lacks %q:\n%s", key, want, b)
			}
		}
	}
	code, b := e.request("GET", runtimeapi.PathInternalOrgs+e.org+"/runtime", controllerToken, nil, nil)
	if code != http.StatusOK || strings.Contains(string(b), `"engineer"`) {
		t.Fatalf("runtime still lists the engineer: %d %s", code, b)
	}
	if code, _ := e.request("POST", runtimeapi.PathLeaseAcquire, eng.token, nil, runtimeapi.LeaseAcquireRequest{}); code != http.StatusForbidden {
		t.Fatalf("retired seat acquired a lease: %d", code)
	}
	// The retired seat's history stays readable in the console: both turns
	// completed and its handoff is kept.
	code, b = e.request("GET", runtimeapi.PathConsoleSeats+engID, consoleToken, nil, nil)
	var detail runtimeapi.ConsoleSeatDetail
	if err := json.Unmarshal(b, &detail); code != http.StatusOK || err != nil {
		t.Fatalf("console seat: %d %s", code, b)
	}
	if detail.Config.RetiredAt == nil || detail.Handoff == nil || detail.Handoff.Objective != "Migrate the database to schema v2" {
		t.Fatalf("retired seat detail = %+v", detail)
	}
	completed := 0
	for _, ex := range detail.Executions {
		if ex.State == "completed" {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("executions = %+v", detail.Executions)
	}

	// A later organisation deletion revokes everything at once.
	code, _ = e.request("DELETE", runtimeapi.PathInternalOrgs+e.org+"?retention=retain", controllerToken, nil, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := e.request("GET", runtimeapi.PathSelf, "tok-lead", nil, nil); code != http.StatusForbidden {
		t.Fatalf("seat of deleted organisation: %d", code)
	}
}

// TestRetirementGraceEnds retires a seat whose turn outlives the grace
// period: the turn is interrupted, its message returned to the sender, and
// the summary says so.
func TestRetirementGraceEnds(t *testing.T) {
	e := newEnvWith(t, false, func(o *platform.Options) { o.RetirementGrace = 300 * time.Millisecond })
	e.sync(orgfixture.Spec())
	lead, eng := e.seat("lead"), e.seat("engineer")
	lead.mustTool("messages.send", map[string]any{"to": "engineer", "body": "a very long task"})
	if eng.next() == nil {
		t.Fatal("engineer got no work")
	}
	e.sync(removeEngineer())
	waitRetired(t, eng)
	d := lead.nextAcked()
	if !strings.Contains(d.Message.Body, "Not delivered") || !strings.Contains(d.Message.Body, "a very long task") {
		t.Fatalf("returned = %s", d.Message.Body)
	}
	sum := e.seat("rep_a").nextAcked().Message.Body
	for _, want := range []string{"grace period ended", "last turn was interrupted", "left no handoff", "a very long task"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("summary lacks %q:\n%s", want, sum)
		}
	}
}

// TestRetirementCancelledByRedeclaring keeps a seat that is declared again
// before it retires.
func TestRetirementCancelledByRedeclaring(t *testing.T) {
	e := newEnvWith(t, false, func(o *platform.Options) { o.RetirementGrace = time.Minute })
	first := e.sync(orgfixture.Spec())
	eng := e.seat("engineer")
	eng.mustTool("memory.write", map[string]any{"store": "engineer", "path": "notes/keep.md", "text": "keep me"})
	e.sync(removeEngineer())
	res := e.sync(orgfixture.Spec())
	if len(res.Retiring) != 0 || res.Seats["engineer"].SeatID != first.Seats["engineer"].SeatID {
		t.Fatalf("redeclared seat: %+v", res)
	}
	// The unseen notice was withdrawn; the seat carries on with its memory.
	if d := eng.next(); d != nil {
		t.Fatalf("unexpected delivery after cancellation: %+v", d.Message)
	}
	if out := eng.mustTool("memory.read", map[string]any{"store": "engineer", "path": "notes/keep.md"}); !strings.Contains(fmt.Sprint(out), "keep me") {
		t.Fatalf("personal memory = %v", out)
	}
	lead := e.seat("lead")
	lead.mustTool("messages.send", map[string]any{"to": "engineer", "body": "welcome back"})
	if d := eng.nextAcked(); d.Message.Body != "welcome back" {
		t.Fatalf("delivery = %+v", d.Message)
	}
}
