//go:build integration

package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/google/uuid"
)

func taskParent(t *testing.T) (*Store, Fence, string) {
	t.Helper()
	s := open(t)
	syn := syncOrg(t, s, orgfixture.Manifest(t))
	f := lease(t, s, syn.Seats["engineer"].SeatID)
	id := uuid.NewString()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) SELECT $1,id,$2,config_revision,'fake' FROM seats WHERE id=$3`, id, f.Generation, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, id
}
func taskRequest() runtimeapi.TaskRunRequest {
	return runtimeapi.TaskRunRequest{RequestID: uuid.NewString(), Prompt: "review", ArtifactPaths: []string{"out.txt"}}
}

func TestTaskRunLifecycleAndAuthority(t *testing.T) {
	ctx := context.Background()
	s, f, parent := taskParent(t)
	req := taskRequest()
	run, err := s.CreateTaskRun(ctx, f, parent, req)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.CreateTaskRun(ctx, f, parent, req)
	if err != nil || same.ID != run.ID {
		t.Fatalf("retry: %+v %v", same, err)
	}
	changed := req
	changed.Prompt = "changed"
	if _, err = s.CreateTaskRun(ctx, f, parent, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if _, err = s.TaskRun(ctx, uuid.NewString(), run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-seat read: %v", err)
	}
	seatID, id, err := s.TaskRunByServiceAccount(ctx, "acme", run.ServiceAccount)
	if err != nil || seatID != f.SeatID || id != run.ID {
		t.Fatalf("worker mapping: %s %s %v", seatID, id, err)
	}
	if _, err = s.SeatByServiceAccount(ctx, "acme", run.ServiceAccount); !errors.Is(err, ErrNotFound) {
		t.Fatal("worker resolves as persistent seat")
	}
	if _, err = s.ClaimTaskRun(ctx, f.SeatID, run.ID, "pod-child"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTaskRun(ctx, f.SeatID, run.ID, "pod-attacker"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Pod claimed: %v", err)
	}
	finish := runtimeapi.TaskRunFinish{State: runtimeapi.TaskCompleted, Result: runtimeapi.TaskRunResult{Output: "done", Artifacts: []runtimeapi.TaskFile{{Path: "out.txt", Content: "result"}}}}
	if err = s.FinishTaskRun(ctx, f.SeatID, run.ID, "pod-attacker", finish); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong Pod finished: %v", err)
	}
	forged := finish
	forged.Result.Artifacts = []runtimeapi.TaskFile{{Path: "secret", Content: "not requested"}}
	if err = s.FinishTaskRun(ctx, f.SeatID, run.ID, "pod-child", forged); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unrequested artifact: %v", err)
	}
	if err = s.FinishTaskRun(ctx, f.SeatID, run.ID, "pod-child", finish); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.TaskRunByServiceAccount(ctx, "acme", run.ServiceAccount); !errors.Is(err, ErrNotFound) {
		t.Fatal("terminal worker still authenticates")
	}
	if err = s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned"); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	var notices int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE external_event_id=$1 AND correlation_id=$2`, "task-result:"+run.ID, run.ID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("notifications=%d, err=%v", notices, err)
	}
	final, err := s.TaskRun(ctx, f.SeatID, run.ID)
	if err != nil || final.CleanedAt == nil || final.Result.Artifacts[0].Content != "result" {
		t.Fatalf("retained result: %+v %v", final, err)
	}
	var executions int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE seat_id=$1`, f.SeatID).Scan(&executions)
	if executions != 1 {
		t.Fatal("worker created a second persistent-seat turn")
	}
}
func TestTaskRunConcurrentAdmissionAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, f, parent := taskParent(t)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.CreateTaskRun(ctx, f, parent, taskRequest()); results <- err }()
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
	if success != runtimeapi.MaxActiveTaskRuns {
		t.Fatalf("admitted %d", success)
	}
	runs, err := s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CancelTaskRun(ctx, Fence{SeatID: f.SeatID, Generation: f.Generation + 1}, runs[0].ID); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale cancel: %v", err)
	}
	if err = s.CancelTaskRun(ctx, f, runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTaskRun(ctx, f, parent, taskRequest()); !errors.Is(err, ErrConflict) {
		t.Fatal("reused capacity before Pod cleanup")
	}
	if err = s.ReconcileTaskRun(ctx, f.SeatID, runs[0].ID, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTaskRun(ctx, f, parent, taskRequest()); err != nil {
		t.Fatal(err)
	}
}
func TestTaskRunDeadlineAndParentRevocation(t *testing.T) {
	ctx := context.Background()
	s, f, parent := taskParent(t)
	run, err := s.CreateTaskRun(ctx, f, parent, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE task_runs SET deadline=now()-interval '1 second' WHERE id=$1`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.TaskRunByServiceAccount(ctx, "acme", run.ServiceAccount); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired worker authenticated")
	}
	runs, err := s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil || runs[0].State != runtimeapi.TaskTimedOut {
		t.Fatalf("deadline: %+v %v", runs, err)
	}
	_ = s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned")
	next, err := s.CreateTaskRun(ctx, f, parent, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE execution_leases SET generation=generation+1 WHERE seat_id=$1`, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.TaskRunByServiceAccount(ctx, "acme", next.ServiceAccount); !errors.Is(err, ErrNotFound) {
		t.Fatal("fenced parent's worker authenticated")
	}
	runs, err = s.ListTaskRuns(ctx, f.SeatID, true)
	if err != nil || runs[0].State != runtimeapi.TaskCancelled {
		t.Fatalf("fence: %+v %v", runs, err)
	}
}

func TestTaskRunLaunchCleanupFence(t *testing.T) {
	ctx := context.Background()
	s, f, parent := taskParent(t)
	run, err := s.CreateTaskRun(ctx, f, parent, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "launch"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "launch"); !errors.Is(err, ErrConflict) {
		t.Fatalf("launch replay: %v", err)
	}
	if err := s.CancelTaskRun(ctx, f, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned"); err == nil {
		t.Fatal("unresolved launch released capacity")
	}
	got, err := s.TaskRun(ctx, f.SeatID, run.ID)
	if err != nil || got.CleanedAt != nil || got.LaunchStartedAt == nil || got.LaunchResolvedAt != nil {
		t.Fatalf("fence not durable: %+v %v", got, err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "launched"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "launch"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleaned run launched: %v", err)
	}
}

func TestTaskRunCancellationBeforeLaunchFencesStaleController(t *testing.T) {
	ctx := context.Background()
	s, f, parent := taskParent(t)
	run, err := s.CreateTaskRun(ctx, f, parent, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelTaskRun(ctx, f, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTaskRun(ctx, f.SeatID, run.ID, "launch"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale controller launched: %v", err)
	}
}
