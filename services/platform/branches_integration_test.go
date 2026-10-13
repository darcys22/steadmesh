//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/store"
)

func TestBranchToolsFromHandoffThroughExplicitSelection(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	rep, lead, stranger := e.seat("rep_a"), e.seat("lead"), e.seat("engineer")
	rep.mustTool("messages.send", map[string]any{"to": "lead", "body": "Explore two approaches"})
	delivery := lead.next()
	if delivery == nil {
		t.Fatal("no parent execution")
	}
	call := func(name string, args any) runtimeapi.RunBranchComparison {
		t.Helper()
		raw, _ := json.Marshal(args)
		code, body := e.request(http.MethodPost, runtimeapi.PathToolCall+name, lead.token,
			map[string]string{runtimeapi.HeaderGeneration: fmt.Sprint(lead.gen), runtimeapi.HeaderExecution: delivery.ExecutionID},
			runtimeapi.ToolCallRequest{Arguments: raw})
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		var res runtimeapi.ToolCallResult
		if err := json.Unmarshal(body, &res); err != nil || res.IsError {
			t.Fatalf("%s: %v %s", name, err, body)
		}
		var group runtimeapi.RunBranchComparison
		if err := json.Unmarshal(res.Content, &group); err != nil {
			t.Fatal(err)
		}
		return group
	}
	group := call("branches.start", runtimeapi.RunBranchRequest{RequestID: uuid.NewString(), Handoff: "Explore a solution without modifying the parent",
		InputFiles:   []runtimeapi.TaskFile{{Path: "context.txt", Content: "shared"}},
		Alternatives: []runtimeapi.RunAlternative{{Name: "a", Prompt: "First approach"}, {Name: "b", Prompt: "Second approach"}}})
	if group.ID == "" || len(group.Alternatives) != 2 {
		t.Fatalf("start: %+v", group)
	}
	st, err := store.Open(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	child := group.Alternatives[0].TaskRunID
	if _, err := st.ClaimTaskRun(context.Background(), e.seats["lead"], child, "test-child"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTaskRun(context.Background(), e.seats["lead"], child, "test-child", runtimeapi.TaskRunFinish{State: runtimeapi.TaskCompleted,
		Result: runtimeapi.TaskRunResult{Output: "Proposed solution A"}}); err != nil {
		t.Fatal(err)
	}
	group = call("branches.compare", map[string]string{"id": group.ID})
	if group.Alternatives[0].OutputPreview != "Proposed solution A" || group.AllTerminal || group.Selection != nil {
		t.Fatalf("compare: %+v", group)
	}
	selected := call("branches.select", runtimeapi.RunBranchSelectRequest{ID: group.ID, TaskRunID: child, ExpectedRevision: group.Revision,
		ExpectedResultSHA256: group.Alternatives[0].ResultSHA256})
	if selected.Selection == nil || selected.Selection.TaskRunID != child || selected.Revision != 2 {
		t.Fatalf("select: %+v", selected)
	}
	if out, isErr := stranger.tool("branches.compare", map[string]string{"id": group.ID}); !isErr || out["error"] != "not_found" {
		t.Fatalf("foreign compare: %v", out)
	}
	// Results and the choice remain accessible after the parent turn ends.
	lead.ack(delivery)
	if later := call("branches.compare", map[string]string{"id": group.ID}); later.Selection == nil || later.Selection.TaskRunID != child {
		t.Fatalf("lost choice: %+v", later)
	}
}
