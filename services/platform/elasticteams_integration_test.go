//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/pkg/taskworker"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestElasticTeamHTTPAuthority(t *testing.T) {
	e := newEnv(t)
	org := orgfixture.Spec()
	org.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 100000, ExecutionOutputTokens: 50000, MaxRequestOutputTokens: 1000}
	org.ElasticTeams = &spec.ElasticTeams{EnabledSeats: []string{"engineer"}, MaxActiveWorkers: 8, MaxActivePerExecution: 8, MaxLaunchesPerExecution: 16, MaxTimeoutSeconds: 600, Templates: map[string]spec.WorkerTemplate{"reader": {Instructions: "Read the supplied evidence carefully.", ToolNames: []string{"memory.read"}}}}
	e.sync(org)
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
	req := runtimeapi.ElasticTeamRequest{RequestID: uuid.NewString(), Workers: []runtimeapi.ElasticWorkerRequest{{Name: "research", Template: "reader", Task: runtimeapi.TaskRunRequest{Prompt: "Read", ToolNames: []string{"memory.read"}}}}}
	code, b := e.request("POST", runtimeapi.PathElasticTeams, parent.token, hdr, req)
	if code != http.StatusCreated {
		t.Fatalf("create %d %s", code, b)
	}
	var team runtimeapi.ElasticTeam
	if err = json.Unmarshal(b, &team); err != nil {
		t.Fatal(err)
	}
	run := team.Workers[0].Run
	e.auth.AddSeat("elastic-worker", auth.Seat{SeatID: e.seats["engineer"], TaskRunID: run.ID, PodUID: "elastic-pod"})
	code, b = e.request("POST", runtimeapi.PathTaskWorker+"/claim", "elastic-worker", nil, map[string]any{})
	if code != 200 {
		t.Fatalf("claim %d %s", code, b)
	}
	var boot taskworker.Bootstrap
	_ = json.Unmarshal(b, &boot)
	if boot.Instructions != "Read the supplied evidence carefully." {
		t.Fatalf("template instructions: %q", boot.Instructions)
	}
	for _, route := range []string{runtimeapi.PathElasticTeams, runtimeapi.PathToolCall + "teams.expand", runtimeapi.PathToolCall + "tasks.start"} {
		code, b = e.request("POST", route, "elastic-worker", hdr, req)
		if code != http.StatusForbidden {
			t.Errorf("nested escape %s %d %s", route, code, b)
		}
	}
	// Remove a tool from the current template without changing the seat profile.
	tmpl := org.ElasticTeams.Templates["reader"]
	tmpl.ToolNames = nil
	org.ElasticTeams.Templates["reader"] = tmpl
	e.sync(org)
	code, b = e.request("POST", runtimeapi.PathToolCall+"memory.read", "elastic-worker", hdr, runtimeapi.ToolCallRequest{Arguments: json.RawMessage(`{"store":"knowledge","path":"a"}`)})
	if code != http.StatusForbidden {
		t.Fatalf("revoked tool still usable: %d %s", code, b)
	}
	// Draining refuses admission but does not revoke current status reads.
	org.ElasticTeams.Draining = true
	e.sync(org)
	req.RequestID = uuid.NewString()
	code, b = e.request("POST", runtimeapi.PathElasticTeams, parent.token, hdr, req)
	if code < 400 {
		t.Fatalf("draining admitted %d %s", code, b)
	}
	// Template removal revokes all worker authority, even bootstrap/tools reads.
	org.ElasticTeams = nil
	e.sync(org)
	code, b = e.request("GET", runtimeapi.PathTools, "elastic-worker", nil, nil)
	if code != http.StatusForbidden {
		t.Fatalf("revoked template retained authority %d %s", code, b)
	}
}
