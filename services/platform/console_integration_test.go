//go:build integration

package platform_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func (e *env) console(path string, out any) {
	e.t.Helper()
	code, b := e.request("GET", path, consoleToken, nil, nil)
	if code != http.StatusOK {
		e.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
}

func TestConsoleAPIIsOffByDefault(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	for _, tok := range []string{consoleToken, controllerToken, ""} {
		if code, _ := e.request("GET", runtimeapi.PathConsoleOrgs, tok, nil, nil); code != http.StatusNotFound {
			t.Fatalf("console API served while disabled: %d", code)
		}
	}
}

func TestConsoleAuthenticationBoundaries(t *testing.T) {
	e := newEnvWithConsole(t, true)
	e.sync(orgfixture.Spec())
	paths := []string{
		runtimeapi.PathConsoleOrgs,
		runtimeapi.PathConsoleOrgs + "/" + e.org + "/seats",
		runtimeapi.PathConsoleOrgs + "/" + e.org + "/activity",
		runtimeapi.PathConsoleSeats + e.seats["lead"],
	}
	for _, p := range paths {
		if code, _ := e.request("GET", p, "", nil, nil); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", p, code)
		}
		if code, _ := e.request("GET", p, "tok-lead", nil, nil); code != http.StatusUnauthorized {
			t.Errorf("seat token on %s: %d", p, code)
		}
		if code, _ := e.request("GET", p, controllerToken, nil, nil); code != http.StatusUnauthorized {
			t.Errorf("controller token on %s: %d", p, code)
		}
		if code, _ := e.request("POST", p, consoleToken, nil, struct{}{}); code < 400 {
			t.Errorf("POST %s accepted: %d", p, code)
		}
	}
	// The console identity cannot use the seat or controller APIs.
	if code, _ := e.request("GET", runtimeapi.PathSelf, consoleToken, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("console token on seat API: %d", code)
	}
	if code, _ := e.request("GET", runtimeapi.PathInternalOrgs+e.org+"/runtime", consoleToken, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("console token on internal API: %d", code)
	}
}

// TestConsoleTracesDelegationAndSurvivesRestart walks the console demo: a
// representative delegates to the lead, the lead runs tools and creates a
// tracker issue, its Pod is replaced, and the seat's history is intact.
func TestConsoleTracesDelegationAndSurvivesRestart(t *testing.T) {
	e := newEnvWithConsole(t, true)
	e.sync(orgfixture.Spec())
	repA, lead := e.seat("rep_a"), e.seat("lead")
	start := time.Now().Add(-time.Second)

	sent := repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "please build the login page"})
	d := lead.next()
	if d == nil {
		t.Fatal("no delivery")
	}
	run1 := d.ExecutionID
	events := []runtimeapi.ExecutionEvent{
		{Kind: "tool_request", CorrelationID: "tu1", Time: time.Now(),
			Data: json.RawMessage(`{"name":"connections.invoke","input":{"connection":"tracker","api_key":"sk-live-123"}}`)},
		{Kind: "tool_result", CorrelationID: "tu1", Time: time.Now(), Data: json.RawMessage(`{"content":"ok"}`)},
	}
	if code, b := lead.do("POST", runtimeapi.PathExecEvents+run1+"/events", events); code != http.StatusNoContent {
		t.Fatalf("events: %d %s", code, b)
	}
	code, b := e.request("POST", runtimeapi.PathToolCall+"connections.invoke", lead.token, map[string]string{
		runtimeapi.HeaderGeneration: jsonInt(lead.gen), runtimeapi.HeaderExecution: run1},
		runtimeapi.ToolCallRequest{Arguments: json.RawMessage(`{"connection":"tracker","operation":"task.write",
			"params":{"title":"login page"},"idempotency_key":"login-1"}`)})
	if code != http.StatusOK {
		t.Fatalf("invoke: %d %s", code, b)
	}
	lead.mustTool("messages.reply", map[string]any{"message_id": d.Message.MessageID, "body": "on it"})
	lead.ack(d)

	// Organisation: configuration without secret values.
	var orgs []runtimeapi.ConsoleOrganization
	e.console(runtimeapi.PathConsoleOrgs, &orgs)
	if len(orgs) != 1 || orgs[0].ID != e.org || len(orgs[0].Seats) == 0 {
		t.Fatalf("orgs = %+v", orgs)
	}
	for _, c := range orgs[0].Connections {
		if c.Key == "tracker" && (c.SecretKind != "k8s" || c.SecretPath != "linear") {
			t.Fatalf("tracker connection = %+v", c)
		}
	}

	// Seats: alive and progressing are reported separately.
	var seats runtimeapi.ConsoleSeatList
	e.console(runtimeapi.PathConsoleOrgs+"/"+e.org+"/seats", &seats)
	var leadStatus *runtimeapi.ConsoleSeatStatus
	for i := range seats.Seats {
		if seats.Seats[i].SeatKey == "lead" {
			leadStatus = &seats.Seats[i]
		}
	}
	if leadStatus == nil || leadStatus.LastProgressAt == nil || leadStatus.Current != nil {
		t.Fatalf("lead status = %+v", leadStatus)
	}

	// Activity: the handoff, the run, its tool call and the operation.
	var act runtimeapi.ConsoleActivity
	e.console(runtimeapi.PathConsoleOrgs+"/"+e.org+"/activity?after="+start.UTC().Format(time.RFC3339Nano), &act)
	kinds := map[string]bool{}
	for _, it := range act.Items {
		kinds[it.Kind] = true
		if it.Kind == runtimeapi.ActivityMessage && it.MessageID == sent["message_id"] {
			if it.SeatKey != "rep_a" || it.PeerSeat != "lead" || it.ExecutionID != run1 {
				t.Fatalf("handoff item = %+v", it)
			}
		}
		if it.Kind == runtimeapi.ActivityToolResult && it.Summary != "connections.invoke" {
			t.Fatalf("tool result not paired with its request: %+v", it)
		}
	}
	for _, k := range []string{runtimeapi.ActivityMessage, runtimeapi.ActivityRunStarted, runtimeapi.ActivityRunFinished,
		runtimeapi.ActivityToolRequest, runtimeapi.ActivityToolResult, runtimeapi.ActivityOperation} {
		if !kinds[k] {
			t.Errorf("activity has no %s: %+v", k, act.Items)
		}
	}
	var latest runtimeapi.ConsoleActivity
	e.console(runtimeapi.PathConsoleOrgs+"/"+e.org+"/activity?limit=2", &latest)
	if len(latest.Items) != 2 || latest.Items[0].At.After(latest.Items[1].At) {
		t.Fatalf("latest activity = %+v", latest.Items)
	}

	// Run detail: trigger, events with credentials withheld, operation, reply.
	var run runtimeapi.ConsoleExecutionDetail
	e.console(runtimeapi.PathConsoleExecutions+run1, &run)
	if run.Trigger == nil || run.Trigger.ID != d.Message.MessageID || run.Execution.State != "completed" {
		t.Fatalf("run = %+v", run.Execution)
	}
	if len(run.Events) != 2 || strings.Contains(string(run.Events[0].Data), "sk-live-123") {
		t.Fatalf("events = %s", run.Events[0].Data)
	}
	if len(run.Operations) != 1 || run.Operations[0].Operation.Operation != "task.write" || run.Operations[0].Status != "succeeded" {
		t.Fatalf("operations = %+v", run.Operations)
	}
	if len(run.Sent) != 1 || run.Sent[0].Body != "on it" || run.Sent[0].RecipientSeat != "rep_a" {
		t.Fatalf("sent = %+v", run.Sent)
	}

	// Conversation: both messages with their deliveries.
	var conv runtimeapi.ConsoleConversationDetail
	e.console(runtimeapi.PathConsoleConversations+sent["conversation_id"].(string), &conv)
	if len(conv.Messages) != 2 || len(conv.Messages[0].Deliveries) != 1 || conv.Messages[0].Deliveries[0].State != "done" {
		t.Fatalf("conversation = %+v", conv.Messages)
	}

	// Work: the tracker operation with its receipt.
	var ops runtimeapi.ConsoleOperationList
	e.console(runtimeapi.PathConsoleOrgs+"/"+e.org+"/operations?seat=lead", &ops)
	if len(ops.Operations) != 1 || ops.Operations[0].ExternalReceipt == "" || ops.Operations[0].ExecutionID != run1 {
		t.Fatalf("operations = %+v", ops.Operations)
	}

	// Replace the lead's Pod: the controller fences it and a new Pod takes
	// the lease at the next generation.
	if code, b := e.request("POST", runtimeapi.PathInternalSeats+e.seats["lead"]+"/fence", controllerToken, nil,
		runtimeapi.FenceRequest{ExpectedGeneration: lead.gen}); code != http.StatusOK {
		t.Fatalf("fence: %d %s", code, b)
	}
	oldGen := lead.gen
	lead = e.seat("lead")
	repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "status?"})
	d2 := lead.next()
	lead.ack(d2)

	var seat runtimeapi.ConsoleSeatDetail
	e.console(runtimeapi.PathConsoleSeats+e.seats["lead"], &seat)
	if seat.Config.SeatID != e.seats["lead"] || len(seat.Executions) != 2 {
		t.Fatalf("seat = %+v", seat)
	}
	if seat.Executions[0].ID != d2.ExecutionID || seat.Executions[0].LeaseGeneration <= oldGen ||
		seat.Executions[1].ID != run1 || seat.Executions[1].LeaseGeneration != oldGen {
		t.Fatalf("runs across the restart = %+v", seat.Executions)
	}
	if len(seat.Memory) == 0 {
		t.Fatalf("seat has no memory metadata: %+v", seat)
	}

	// Nothing the console API returns carries a resolved secret value.
	for _, p := range []string{runtimeapi.PathConsoleOrgs, runtimeapi.PathConsoleExecutions + run1,
		runtimeapi.PathConsoleOrgs + "/" + e.org + "/operations"} {
		if _, b := e.request("GET", p, consoleToken, nil, nil); strings.Contains(string(b), `"test"`) {
			t.Errorf("%s exposes a secret value: %s", p, b)
		}
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
