//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/pkg/taskworker"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestTaskWorkerHTTPIsolation(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	parent := e.seat("engineer")
	execID := uuid.NewString()
	db, err := pgx.Connect(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	_, err = db.Exec(context.Background(), `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) SELECT $1,id,$2,config_revision,'fake' FROM seats WHERE id=$3`, execID, parent.gen, e.seats["engineer"])
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{runtimeapi.HeaderGeneration: fmt.Sprint(parent.gen), runtimeapi.HeaderExecution: execID}
	code, b := e.request("POST", runtimeapi.PathTaskRuns, parent.token, hdr, runtimeapi.TaskRunRequest{RequestID: uuid.NewString(), Prompt: "review", ArtifactPaths: []string{"result.txt"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var run runtimeapi.TaskRun
	if err = json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	e.auth.AddSeat("worker", auth.Seat{SeatID: e.seats["engineer"], TaskRunID: run.ID, PodUID: "worker-pod"})
	for _, route := range []struct{ method, path string }{{"POST", runtimeapi.PathLeaseAcquire}, {"POST", runtimeapi.PathLeaseRelease}, {"GET", runtimeapi.PathInboxNext}, {"GET", runtimeapi.PathBootstrap}, {"GET", runtimeapi.PathSelf}, {"GET", runtimeapi.PathAccess}, {"POST", runtimeapi.PathCredentials + "model"}, {"POST", runtimeapi.PathTaskRuns}, {"PUT", runtimeapi.PathCheckpoint}} {
		code, b = e.request(route.method, route.path, "worker", hdr, map[string]any{})
		if code != http.StatusForbidden {
			t.Errorf("worker escaped to %s: %d %s", route.path, code, b)
		}
	}
	code, b = e.request("POST", runtimeapi.PathTaskWorker+"/claim", "worker", nil, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("claim: %d %s", code, b)
	}
	var boot taskworker.Bootstrap
	_ = json.Unmarshal(b, &boot)
	if boot.Identity.Recovery.Session != nil || boot.Identity.Recovery.Handoff != nil || boot.Manifest.Access != nil {
		t.Fatal("worker inherited parent session/access")
	}
	code, b = e.request("GET", runtimeapi.PathTools, "worker", nil, nil)
	if code != 200 || string(b) != "[]\n" {
		t.Fatalf("default-deny tools: %d %s", code, b)
	}
	for _, name := range []string{"tasks.start", "messages.send", "connections.invoke", "memory.write", "memory.read"} {
		code, b = e.request("POST", runtimeapi.PathToolCall+name, "worker", hdr, runtimeapi.ToolCallRequest{Arguments: json.RawMessage(`{}`)})
		if code != http.StatusForbidden {
			t.Errorf("tool widened: %s %d %s", name, code, b)
		}
	}
	e.auth.AddSeat("other-pod", auth.Seat{SeatID: e.seats["engineer"], TaskRunID: run.ID, PodUID: "other-pod"})
	code, _ = e.request("GET", runtimeapi.PathTools, "other-pod", nil, nil)
	if code != http.StatusForbidden {
		t.Fatal("different Pod used worker identity")
	}
	code, b = e.request("POST", runtimeapi.PathTaskWorker+"/finish", "worker", nil, runtimeapi.TaskRunFinish{State: runtimeapi.TaskCompleted, Result: runtimeapi.TaskRunResult{Output: "reviewed", Artifacts: []runtimeapi.TaskFile{{Path: "result.txt", Content: "safe"}}}})
	if code != http.StatusNoContent {
		t.Fatalf("finish: %d %s", code, b)
	}
	code, _ = e.request("GET", runtimeapi.PathTools, "worker", nil, nil)
	if code != http.StatusForbidden {
		t.Fatal("completed worker still has authority")
	}
	code, b = parent.do("GET", runtimeapi.PathTaskRuns+"/"+run.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	var final runtimeapi.TaskRun
	_ = json.Unmarshal(b, &final)
	if final.State != runtimeapi.TaskCompleted || final.Result.Artifacts[0].Content != "safe" {
		t.Fatalf("result missing: %+v", final)
	}
	other := e.seat("lead")
	code, _ = other.do("GET", runtimeapi.PathTaskRuns+"/"+run.ID, nil)
	if code != http.StatusNotFound {
		t.Fatal("another seat read child result")
	}
}

// This is the cross-feature seam: task identity supplies accounting context,
// so an untrusted harness cannot reset its budget by forging request headers.
func TestTaskWorkerSharesParentBudgetAfterParentCompletes(t *testing.T) {
	model := &budgetModel{}
	e := newEnvWith(t, false, func(o *platform.Options) {
		o.Factories.Model = map[string]func(connectors.Config) (connectors.Model, error){"anthropic": func(connectors.Config) (connectors.Model, error) { return model, nil }}
	})
	sp := orgfixture.Spec()
	sp.Connections["model"] = spec.Connection{Adapter: "anthropic", SecretRef: "k8s:anthropic"}
	sp.HarnessProfiles["claude"] = spec.HarnessProfile{Adapter: "claude-code", ImageDigest: "seat:dev", Model: &spec.ModelSelection{Connection: "model", ID: "claude-x"}}
	leadSpec := sp.Seats["lead"]
	leadSpec.HarnessProfile = "claude"
	sp.Seats["lead"] = leadSpec
	sp.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 20, SeatDailyOutputTokens: 20, ExecutionOutputTokens: 6, MaxRequestOutputTokens: 2}
	e.sync(sp)
	parent := e.seat("lead")
	code, b := e.request("POST", runtimeapi.PathInternalSeats+e.seats["lead"]+"/probe", controllerToken, nil, struct{}{})
	if code != 200 && code != 202 {
		t.Fatalf("probe: %d %s", code, b)
	}
	d := parent.next()
	if d == nil {
		t.Fatal("missing parent execution")
	}
	hdr := map[string]string{runtimeapi.HeaderGeneration: fmt.Sprint(parent.gen), runtimeapi.HeaderExecution: d.ExecutionID}
	runs := []runtimeapi.TaskRun{}
	for i := 0; i < 2; i++ {
		code, b = e.request("POST", runtimeapi.PathTaskRuns, parent.token, hdr, runtimeapi.TaskRunRequest{RequestID: uuid.NewString(), Prompt: "analyse"})
		if code != 201 {
			t.Fatalf("start: %d %s", code, b)
		}
		var run runtimeapi.TaskRun
		_ = json.Unmarshal(b, &run)
		runs = append(runs, run)
		token := fmt.Sprintf("budget-worker-%d", i)
		e.auth.AddSeat(token, auth.Seat{SeatID: e.seats["lead"], TaskRunID: run.ID, PodUID: token})
		code, b = e.request("POST", runtimeapi.PathTaskWorker+"/claim", token, nil, struct{}{})
		if code != 200 {
			t.Fatalf("claim: %d %s", code, b)
		}
	}
	forged := uuid.NewString()
	hostileHeaders := map[string]string{runtimeapi.HeaderExecution: forged, runtimeapi.HeaderGeneration: "987654"}
	infer := func(token string, headers map[string]string) (int, []byte) {
		return e.request("POST", "/v1/model/model/v1/messages", token, headers, map[string]any{"model": "claude-x", "max_tokens": 2})
	}
	if code, b = infer("budget-worker-0", hostileHeaders); code != 200 {
		t.Fatalf("child infer: %d %s", code, b)
	}
	if code, b = infer(parent.token, hdr); code != 200 {
		t.Fatalf("parent infer: %d %s", code, b)
	}
	parent.ack(d)
	if code, b = parent.do("POST", runtimeapi.PathLeaseRelease, struct{}{}); code != 204 {
		t.Fatalf("release: %d %s", code, b)
	}
	if code, b = infer("budget-worker-1", hostileHeaders); code != 200 {
		t.Fatalf("idle-parent child infer: %d %s", code, b)
	}
	if code, b = infer("budget-worker-0", hostileHeaders); code != 429 {
		t.Fatalf("shared exhaustion: %d %s", code, b)
	}
	if model.calls.Load() != 3 {
		t.Fatalf("provider calls=%d", model.calls.Load())
	}
	db, err := pgx.Connect(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	var charged int64
	var forgedCounters int
	if err = db.QueryRow(context.Background(), `SELECT charged_output_tokens FROM model_budget_counters WHERE scope='execution' AND scope_id=$1`, d.ExecutionID).Scan(&charged); err != nil || charged != 6 {
		t.Fatalf("parent charged=%d, err=%v", charged, err)
	}
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM model_budget_counters WHERE scope='execution' AND scope_id=$1`, forged).Scan(&forgedCounters)
	if forgedCounters != 0 {
		t.Fatal("forged execution got a counter")
	}
	e.auth.AddSeat("wrong-pod", auth.Seat{SeatID: e.seats["lead"], TaskRunID: runs[0].ID, PodUID: "wrong"})
	if code, _ = infer("wrong-pod", hostileHeaders); code != 403 {
		t.Fatal("wrong Pod reached model path")
	}
	if code, b = parent.do("POST", runtimeapi.PathTaskRuns+"/"+runs[0].ID+"/cancel", struct{}{}); code != 204 {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if code, _ = infer("budget-worker-0", hostileHeaders); code != 403 {
		t.Fatal("cancelled worker reached budget/model path")
	}
	_, err = db.Exec(context.Background(), `UPDATE task_runs SET deadline=now()-interval '1 second' WHERE id=$1`, runs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ = infer("budget-worker-1", hostileHeaders); code != 403 {
		t.Fatal("expired worker reached budget/model path")
	}
	if model.calls.Load() != 3 {
		t.Fatal("revoked worker reached provider")
	}
}
