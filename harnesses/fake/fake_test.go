package fake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Options{
		New:       func() harnesses.Adapter { return New() },
		QuickBody: "hello there",
		SlowBody:  "/sleep 60s",
	})
}

type rig struct {
	ts   *conformance.ToolServer
	dirs conformance.Dirs
	a    *Adapter
	env  harnesses.Environment
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ts := conformance.NewToolServer(t, "tok")
	dirs := conformance.NewDirs(t, "tok")
	env := conformance.Env(dirs, ts, conformance.BuildTools(t))
	env.SeatKey = "rep_alice"
	r := &rig{ts: ts, dirs: dirs, env: env}
	r.restart(t)
	return r
}

func (r *rig) restart(t *testing.T) harnesses.ResumeResult {
	t.Helper()
	if r.a != nil {
		_ = r.a.Stop(context.Background())
	}
	r.a = New()
	go func(a *Adapter) {
		for range a.Events() {
		}
	}(r.a)
	if err := r.a.Prepare(context.Background(), r.env); err != nil {
		t.Fatal(err)
	}
	res, err := r.a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.a.Stop(context.Background()) })
	return res
}

func (r *rig) deliver(t *testing.T, m runtimeapi.Envelope) harnesses.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := r.a.Deliver(ctx, harnesses.Delivery{DeliveryID: 1, ExecutionID: "e-" + m.MessageID, Message: m})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func args(t *testing.T, c conformance.ToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Arguments, &m); err != nil {
		t.Fatalf("args %s: %v", c.Arguments, err)
	}
	return m
}

func TestPlainTextAck(t *testing.T) {
	r := newRig(t)
	body := strings.Repeat("x", 300)
	tr := r.deliver(t, runtimeapi.Envelope{MessageID: "h1", Origin: "human", Binding: "alice", Body: body})
	if tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", tr)
	}
	calls := r.ts.Calls()
	if len(calls) != 1 || calls[0].Name != "messages.reply" {
		t.Fatalf("calls %+v", calls)
	}
	a := args(t, calls[0])
	if a["message_id"] != "h1" || a["body"] != "ack: "+strings.Repeat("x", 200) {
		t.Fatalf("reply args %v", a)
	}
	if calls[0].Generation != "3" {
		t.Fatalf("generation header %q", calls[0].Generation)
	}
}

func TestScriptCommands(t *testing.T) {
	r := newRig(t)
	body := strings.Join([]string{
		"some prose that is ignored",
		`/tool memory.write {"store":"rep_alice","path":"notes/t.md","text":"b"}`,
		"/file write notes/n.txt hello world",
		"/file read notes/n.txt",
		"/file read ../../etc/passwd",
		"/sleep 10ms",
		"/reply done here",
		"/claim report finished",
		"/bogus",
	}, "\n")
	tr := r.deliver(t, runtimeapi.Envelope{MessageID: "h2", Origin: "human", Body: body})
	if tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", tr)
	}
	calls := r.ts.Calls()
	if len(calls) != 2 || calls[0].Name != "memory.write" || calls[1].Name != "messages.reply" {
		t.Fatalf("calls %+v", calls)
	}
	if a := args(t, calls[0]); a["store"] != "rep_alice" || a["path"] != "notes/t.md" {
		t.Fatalf("memory.write args %v", a)
	}
	if a := args(t, calls[1]); a["message_id"] != "h2" || a["body"] != "done here" {
		t.Fatalf("reply args %v", a)
	}
	b, err := os.ReadFile(filepath.Join(r.dirs.Workspace, "notes/n.txt"))
	if err != nil || string(b) != "hello world" {
		t.Fatalf("workspace file %q %v", b, err)
	}
	// "../../etc/passwd" is confined to the workspace: it resolves to
	// <workspace>/etc/passwd, which does not exist.
	for _, want := range []string{"file notes/n.txt: hello world", "slept 10ms", "claimed: report finished", "error: unknown command /bogus", "error: /file read"} {
		if !strings.Contains(tr.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, tr.Output)
		}
	}
	if tr.Claim == nil || tr.Claim.Text != "report finished" {
		t.Fatalf("claim %+v", tr.Claim)
	}
}

func TestScriptedFailure(t *testing.T) {
	r := newRig(t)
	tr := r.deliver(t, runtimeapi.Envelope{MessageID: "h3", Origin: "human", Body: "/fail boom\n/reply never"})
	if tr.Status != harnesses.TurnFailed || !strings.Contains(tr.Error, "boom") {
		t.Fatalf("%+v", tr)
	}
	if len(r.ts.Calls()) != 0 {
		t.Fatal("commands after /fail must not run")
	}
}

func TestDelegationRoundTrip(t *testing.T) {
	rep := newRig(t)
	// 1. Human asks the representative to delegate.
	tr := rep.deliver(t, runtimeapi.Envelope{MessageID: "h10", Origin: "human", Binding: "alice", Body: "/delegate reviewer /task\n/file write review.txt ok\n/reply looks good"})
	if tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", tr)
	}
	calls := rep.ts.Calls()
	if len(calls) != 1 || calls[0].Name != "messages.send" {
		t.Fatalf("calls %+v", calls)
	}
	sa := args(t, calls[0])
	if sa["to"] != "reviewer" || sa["correlation_id"] != "h10" || sa["body"] != "/task\n/file write review.txt ok\n/reply looks good" {
		t.Fatalf("send args %v", sa)
	}
	st := rep.a.loadState()
	if st.Delegations["h10"].HumanMessageID != "h10" || st.Delegations["sent-1"].DelegatedTo != "reviewer" {
		t.Fatalf("state %+v", st)
	}

	// 2. The reviewer (another fake seat) executes the task and replies.
	rev := newRig(t)
	rtr := rev.deliver(t, runtimeapi.Envelope{MessageID: "sent-1", Origin: "seat", SenderSeat: "rep_alice", CorrelationID: "h10", Body: sa["body"].(string)})
	if rtr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", rtr)
	}
	rc := rev.ts.Calls()
	// /reply inside the task, then the task-result reply.
	if len(rc) != 2 || rc[1].Name != "messages.reply" {
		t.Fatalf("reviewer calls %+v", rc)
	}
	ra := args(t, rc[1])
	if ra["message_id"] != "sent-1" || !strings.Contains(ra["body"].(string), "wrote review.txt") {
		t.Fatalf("task reply %v", ra)
	}

	// 3. Restart the representative (state must persist), then deliver the reply.
	if res := rep.restart(t); res.Mode != harnesses.RecoveryNativeResume {
		t.Fatalf("restart mode %q", res.Mode)
	}
	ftr := rep.deliver(t, runtimeapi.Envelope{MessageID: "r1", Origin: "seat", SenderSeat: "reviewer", ParentID: "sent-1", Body: ra["body"].(string)})
	if ftr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", ftr)
	}
	calls = rep.ts.Calls()
	fa := args(t, calls[len(calls)-1])
	if calls[len(calls)-1].Name != "messages.reply" || fa["message_id"] != "h10" || !strings.HasPrefix(fa["body"].(string), "Update from reviewer:") {
		t.Fatalf("forward %+v %v", calls[len(calls)-1], fa)
	}
	if len(rep.a.loadState().Delegations) != 0 {
		t.Fatal("delegation not cleared after forwarding")
	}
	if s := rep.a.SessionSnapshot(); s.Turns != 2 || s.LastMessageIDs[len(s.LastMessageIDs)-1] != "r1" {
		t.Fatalf("session %+v", s)
	}
}

func TestSeatReplyNotAcked(t *testing.T) {
	r := newRig(t)
	r.deliver(t, runtimeapi.Envelope{MessageID: "x1", Origin: "seat", SenderSeat: "b", ParentID: "p", Body: "ack: hi"})
	if n := len(r.ts.Calls()); n != 0 {
		t.Fatalf("an uncorrelated seat reply must not be acked (ack loops); %d calls", n)
	}
}

func TestToolErrorIsVisible(t *testing.T) {
	r := newRig(t)
	r.ts.Handler = func(name string, _ json.RawMessage) runtimeapi.ToolCallResult {
		return runtimeapi.ToolCallResult{IsError: true, Content: json.RawMessage(`{"error":"forbidden store"}`)}
	}
	tr := r.deliver(t, runtimeapi.Envelope{MessageID: "h4", Origin: "human", Body: `/tool memory.write {"store":"rep_bob"}`})
	if tr.Status != harnesses.TurnCompleted || !strings.Contains(tr.Output, "tool memory.write error:") {
		t.Fatalf("%+v", tr)
	}
}

func TestSessionPersistsAcrossRestart(t *testing.T) {
	r := newRig(t)
	r.deliver(t, runtimeapi.Envelope{MessageID: "a", Origin: "human", Body: "one"})
	r.deliver(t, runtimeapi.Envelope{MessageID: "b", Origin: "human", Body: "two"})
	id := r.a.SessionSnapshot().SessionID
	r.restart(t)
	s := r.a.SessionSnapshot()
	if s.SessionID != id || s.Turns != 2 || strings.Join(s.LastMessageIDs, ",") != "a,b" {
		t.Fatalf("session after restart %+v", s)
	}
	if _, err := os.Stat(filepath.Join(r.dirs.Home, SessionFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestNestedTaskForwardsTheEndResult(t *testing.T) {
	lead := newRig(t)
	// A task that is passed on does not reply with an interim result...
	tr := lead.deliver(t, runtimeapi.Envelope{MessageID: "rep-1", Origin: "seat", SenderSeat: "rep_alice", CorrelationID: "h1",
		Body: "/task\n/delegate engineer /task\n/reply built"})
	if tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v", tr)
	}
	calls := lead.ts.Calls()
	if len(calls) != 1 || calls[0].Name != "messages.send" || args(t, calls[0])["to"] != "engineer" {
		t.Fatalf("lead calls %+v", calls)
	}
	// ...the downstream result is forwarded to the original sender instead.
	lead.deliver(t, runtimeapi.Envelope{MessageID: "eng-1", Origin: "seat", SenderSeat: "engineer", ParentID: "sent-1",
		Body: "task result from engineer: built"})
	calls = lead.ts.Calls()
	fa := args(t, calls[len(calls)-1])
	if calls[len(calls)-1].Name != "messages.reply" || fa["message_id"] != "rep-1" || !strings.Contains(fa["body"].(string), "built") {
		t.Fatalf("forward %+v %v", calls[len(calls)-1], fa)
	}
}

func TestPlatformNotices(t *testing.T) {
	r := newRig(t)
	tr := r.deliver(t, runtimeapi.Envelope{MessageID: "s1", Origin: "system",
		Body: "Not delivered: seat x is retiring.\n\nYour message:\n> /fail quoted text is not run"})
	if tr.Status != harnesses.TurnCompleted || len(r.ts.Calls()) != 0 {
		t.Fatalf("notice turn %+v, calls %+v", tr, r.ts.Calls())
	}
	tr = r.deliver(t, runtimeapi.Envelope{MessageID: "s2", Origin: "system", Body: "Retirement notice: seat x was removed."})
	calls := r.ts.Calls()
	if tr.Status != harnesses.TurnCompleted || len(calls) != 1 || calls[0].Name != "handoff.update" {
		t.Fatalf("retirement turn %+v, calls %+v", tr, calls)
	}
	if a := args(t, calls[0]); !strings.Contains(a["objective"].(string), "Hand over") {
		t.Fatalf("handoff %v", a)
	}
}
