//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/google/uuid"
)

func elasticParent(t *testing.T, active, launches int) (*Store, Fence, string, string) {
	t.Helper()
	s := open(t)
	p := orgfixture.Spec()
	p.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 100000, ExecutionOutputTokens: 50000, MaxRequestOutputTokens: 100}
	p.ElasticTeams = &spec.ElasticTeams{EnabledSeats: []string{"engineer", "reviewer"}, MaxActiveWorkers: active, MaxActivePerExecution: active, MaxLaunchesPerExecution: launches, MaxTimeoutSeconds: 600, Templates: map[string]spec.WorkerTemplate{"research": {Instructions: "Research only", HarnessProfile: "fake", ExecutionProfile: "interactive", ToolNames: []string{"memory.read"}}}}
	m, err := compile.Compile(p, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	syn := syncOrg(t, s, m)
	f := lease(t, s, syn.Seats["engineer"].SeatID)
	id := elasticExecution(t, s, f)
	return s, f, id, syn.Seats["reviewer"].SeatID
}
func elasticExecution(t *testing.T, s *Store, f Fence) string {
	t.Helper()
	id := uuid.NewString()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) SELECT $1,id,$2,config_revision,'fake' FROM seats WHERE id=$3`, id, f.Generation, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func elasticRequest(n int) runtimeapi.ElasticTeamRequest {
	req := runtimeapi.ElasticTeamRequest{RequestID: uuid.NewString()}
	for i := 0; i < n; i++ {
		req.Workers = append(req.Workers, runtimeapi.ElasticWorkerRequest{Name: fmt.Sprintf("worker-%d", i), Template: "research", Task: runtimeapi.TaskRunRequest{Prompt: "Read and summarize"}})
	}
	return req
}
func TestElasticTeamAtomicRetryCancel(t *testing.T) {
	ctx := context.Background()
	s, f, id, _ := elasticParent(t, 10, 20)
	req := elasticRequest(10)
	team, err := s.CreateElasticTeam(ctx, f, id, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(team.Workers) != 10 {
		t.Fatal(team)
	}
	same, err := s.CreateElasticTeam(ctx, f, id, req)
	if err != nil || same.ID != team.ID {
		t.Fatalf("retry: %+v %v", same, err)
	}
	if _, err = s.CreateElasticTeam(ctx, f, elasticExecution(t, s, f), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution replay: %v", err)
	}
	changed := elasticRequest(10)
	changed.RequestID = req.RequestID
	if _, err = s.CreateElasticTeam(ctx, f, id, changed); err != nil {
		t.Fatal(err)
	}
	changed.Workers[0].Task.Prompt = "different"
	if _, err = s.CreateElasticTeam(ctx, f, id, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("payload replay: %v", err)
	}
	if _, err = s.GetElasticTeam(ctx, uuid.NewString(), team.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("isolation: %v", err)
	}
	if err = s.CancelElasticTeam(ctx, f, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTaskRun(ctx, f, id, taskRequest()); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncleaned released: %v", err)
	}
	for _, w := range team.Workers {
		if err = s.ReconcileTaskRun(ctx, f.SeatID, w.Run.ID, "cleaned"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.CreateElasticTeam(ctx, f, id, elasticRequest(10)); err != nil {
		t.Fatal(err)
	}
}
func TestElasticTeamRollbackAndLegacyLimits(t *testing.T) {
	ctx := context.Background()
	s, f, id, _ := elasticParent(t, 2, 4)
	if _, err := s.CreateElasticTeam(ctx, f, id, elasticRequest(3)); !errors.Is(err, ErrConflict) {
		t.Fatalf("over-capacity group: %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_runs WHERE parent_execution_id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial group: %d %v", n, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.CreateTaskRun(ctx, f, id, taskRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateElasticTeam(ctx, f, id, elasticRequest(1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy bypass: %v", err)
	}
	forged := taskRequest()
	forged.Template = "research"
	if _, err := s.CreateTaskRun(ctx, f, id, forged); !errors.Is(err, ErrInvalid) {
		t.Fatalf("template bypass: %v", err)
	}
}
func TestElasticTeamConcurrentOrganizationAdmission(t *testing.T) {
	ctx := context.Background()
	s, f, id, other := elasticParent(t, 3, 10)
	f2 := lease(t, s, other)
	id2 := elasticExecution(t, s, f2)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fence, parent := f, id
			if i%2 != 0 {
				fence, parent = f2, id2
			}
			_, err := s.CreateTaskRun(ctx, fence, parent, taskRequest())
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 3 {
		t.Fatalf("admitted %d, want3", success)
	}
}
func TestElasticTeamBudgetInflightAndDrain(t *testing.T) {
	ctx := context.Background()
	s, f, id, _ := elasticParent(t, 5, 10)
	var org string
	if err := s.pool.QueryRow(ctx, `SELECT organization_id FROM seats WHERE id=$1`, f.SeatID).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,model_budgets,execution_output_tokens}','200') WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	reservation, err := s.ReserveModelOutput(ctx, org, f, id, false, 150, "")
	if err != nil {
		t.Fatal(err)
	}
	if reservation.OutputTokens != 100 {
		t.Fatal(reservation)
	}
	if _, err = s.CreateElasticTeam(ctx, f, id, elasticRequest(2)); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "headroom") {
		t.Fatalf("inflight headroom: %v", err)
	}
	if _, err = s.CreateElasticTeam(ctx, f, id, elasticRequest(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,elastic_teams,draining}','true') WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTaskRun(ctx, f, id, taskRequest()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("drain bypass: %v", err)
	}
}

func TestElasticTeamToolsAndPolicyRevocation(t *testing.T) {
	ctx := context.Background()
	s, f, id, _ := elasticParent(t, 3, 10)
	req := elasticRequest(2)
	req.Workers[1].Task.ToolNames = []string{}
	team, err := s.CreateElasticTeam(ctx, f, id, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range team.Workers {
		if len(w.Run.Request.ToolNames) != 0 {
			t.Fatalf("omitted/empty grants widened: %+v", w)
		}
	}
	bad := elasticRequest(1)
	bad.Workers[0].Task.ToolNames = []string{"memory.search"}
	if _, err = s.CreateElasticTeam(ctx, f, id, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("template ceiling: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,elastic_teams,draining}','true') WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.State != runtimeapi.TaskPending {
			t.Fatal("drain cancelled existing worker")
		}
	}
	if _, err = s.pool.Exec(ctx, `UPDATE organizations SET manifest=manifest #- '{spec,elastic_teams,templates,research}' WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	runs, err = s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.State != runtimeapi.TaskCancelled {
			t.Fatal("removed template did not cancel")
		}
	}
}

func TestElasticTeamFailedLaunchesAndBranchesCount(t *testing.T) {
	ctx := context.Background()
	s, f, id, _ := elasticParent(t, 3, 3)
	branch, err := s.CreateRunBranch(ctx, f, id, branchRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateElasticTeam(ctx, f, id, elasticRequest(2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("branches escaped aggregate: %v", err)
	}
	for _, a := range branch.Alternatives {
		if err = s.ReconcileTaskRun(ctx, f.SeatID, a.TaskRunID, "failed"); err != nil {
			t.Fatal(err)
		}
		if err = s.ReconcileTaskRun(ctx, f.SeatID, a.TaskRunID, "cleaned"); err != nil {
			t.Fatal(err)
		}
	}
	team, err := s.CreateElasticTeam(ctx, f, id, elasticRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CancelElasticTeam(ctx, f, team.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileTaskRun(ctx, f.SeatID, team.Workers[0].Run.ID, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTaskRun(ctx, f, id, taskRequest()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "launch limit") {
		t.Fatalf("cleaned failed launches forgotten: %v", err)
	}
}

func TestElasticTeamInheritedProfilesSurviveCleanupPoll(t *testing.T) {
	ctx := context.Background()
	s, f, execution, _ := elasticParent(t, 2, 4)
	// Omitting selectors means inherit the requesting seat's profiles.
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET manifest=manifest #- '{spec,elastic_teams,templates,research,harness_profile}' #- '{spec,elastic_teams,templates,research,execution_profile}' WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateElasticTeam(ctx, f, execution, elasticRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != runtimeapi.TaskPending {
		t.Fatalf("inherited profiles revoked by cleanup: %+v", runs)
	}
	if _, err := s.ClaimTaskRun(ctx, f.SeatID, team.Workers[0].Run.ID, "inherited-profile-pod"); err != nil {
		t.Fatalf("inherited-profile worker could not claim: %v", err)
	}
	// Explicit mismatches must still revoke, even after a successful claim.
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,elastic_teams,templates,research,harness_profile}','"different"') WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	runs, err = s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != runtimeapi.TaskCancelled {
		t.Fatalf("explicit mismatch not revoked: %+v", runs)
	}
}

func TestElasticPerExecutionLimitDoesNotBecomeSeatLimit(t *testing.T) {
	ctx := context.Background()
	s, f, first, _ := elasticParent(t, 3, 4)
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,elastic_teams,max_active_per_execution}','2') WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) SELECT $1,id,$2,config_revision,'fake' FROM seats WHERE id=$3`, second, f.Generation, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateElasticTeam(ctx, f, first, elasticRequest(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateElasticTeam(ctx, f, second, elasticRequest(1)); err != nil {
		t.Fatalf("second execution should fit org capacity: %v", err)
	}
	if _, err := s.CreateElasticTeam(ctx, f, second, elasticRequest(1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("organization cap not enforced: %v", err)
	}
}

func TestElasticOmittedTimeoutUsesPolicyCeiling(t *testing.T) {
	ctx := context.Background()
	s, f, execution, _ := elasticParent(t, 3, 4)
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET manifest=jsonb_set(manifest,'{spec,elastic_teams,max_timeout_seconds}','60') WHERE id=(SELECT organization_id FROM seats WHERE id=$1)`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	req := elasticRequest(1)
	req.Workers[0].Task.TimeoutSeconds = 0
	team, err := s.CreateElasticTeam(ctx, f, execution, req)
	if err != nil {
		t.Fatal(err)
	}
	if team.Workers[0].Run.Request.TimeoutSeconds != 60 {
		t.Fatal("omitted timeout did not honor policy")
	}
	req = elasticRequest(1)
	req.Workers[0].Task.TimeoutSeconds = 61
	if _, err = s.CreateElasticTeam(ctx, f, execution, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("explicit oversized timeout accepted: %v", err)
	}
}
