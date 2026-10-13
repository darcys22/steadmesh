//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func branchFixture(t *testing.T) (*Store, Fence, string, string) {
	t.Helper()
	s := open(t)
	org := syncOrg(t, s, orgfixture.Manifest(t))
	f := lease(t, s, org.Seats["lead"].SeatID)
	execution := uuid.NewString()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO executions
		(id, seat_id, lease_generation, config_revision, harness_adapter)
		VALUES ($1, $2, $3, 'test', 'fake')`, execution, f.SeatID, f.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, execution, org.Seats["engineer"].SeatID
}

func branchRequest() runtimeapi.RunBranchRequest {
	return runtimeapi.RunBranchRequest{RequestID: uuid.NewString(), Handoff: "Implement a deterministic formatter.",
		InputFiles: []runtimeapi.TaskFile{{Path: "fixture.txt", Content: "same starting point"}}, ArtifactPaths: []string{"result.txt"},
		Alternatives: []runtimeapi.RunAlternative{{Name: "simple", Prompt: "Try direct formatting"}, {Name: "table", Prompt: "Try a table-driven implementation"}}}
}

func completeAlternative(t *testing.T, s *Store, seatID, id, output string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ClaimTaskRun(ctx, seatID, id, "pod-"+id); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTaskRun(ctx, seatID, id, "pod-"+id, runtimeapi.TaskRunFinish{State: runtimeapi.TaskCompleted,
		Result: runtimeapi.TaskRunResult{Output: output, Artifacts: []runtimeapi.TaskFile{{Path: "result.txt", Content: output}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestRunBranchLifecycleAndSelection(t *testing.T) {
	ctx := context.Background()
	s, f, execution, stranger := branchFixture(t)
	req := branchRequest()
	group, err := s.CreateRunBranch(ctx, f, execution, req)
	if err != nil {
		t.Fatal(err)
	}
	if group.Revision != 1 || group.AllTerminal || group.Selection != nil || len(group.Alternatives) != 2 || group.InputFileCount != 1 {
		t.Fatalf("created %+v", group)
	}
	first := group.Alternatives[0].TaskRunID
	second := group.Alternatives[1].TaskRunID
	a, err := s.TaskRun(ctx, f.SeatID, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.TaskRun(ctx, f.SeatID, second)
	if err != nil {
		t.Fatal(err)
	}
	if a.Request.InputFiles[0].Content != b.Request.InputFiles[0].Content || a.Request.Prompt == b.Request.Prompt || len(a.Request.ToolNames) != 0 || a.ParentExecutionID != execution {
		t.Fatalf("independent explicit inputs: %+v %+v", a.Request, b.Request)
	}
	// Caller memory changes do not change the durable snapshot.
	req.InputFiles[0].Content = "changed after start"
	a, _ = s.TaskRun(ctx, f.SeatID, first)
	if a.Request.InputFiles[0].Content != "same starting point" {
		t.Fatal("stored input changed")
	}
	if _, err := s.CompareRunBranch(ctx, stranger, group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-seat comparison: %v", err)
	}
	choice := runtimeapi.RunBranchSelectRequest{ID: group.ID, TaskRunID: first, ExpectedRevision: 1, ExpectedResultSHA256: strings.Repeat("0", 64)}
	if _, err := s.SelectRunBranch(ctx, f, choice); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pending selection: %v", err)
	}
	output := strings.Repeat("é", 500)
	completeAlternative(t, s, f.SeatID, first, output)
	group, err = s.CompareRunBranch(ctx, f.SeatID, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if group.AllTerminal || !group.Alternatives[0].OutputTruncated || group.Alternatives[0].ArtifactCount != 1 || group.Alternatives[0].ArtifactBytes != len(output) {
		t.Fatalf("comparison: %+v", group)
	}
	if _, err := s.SelectRunBranch(ctx, f, choice); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest: %v", err)
	}
	choice.ExpectedResultSHA256 = group.Alternatives[0].ResultSHA256
	selected, err := s.SelectRunBranch(ctx, f, choice)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Revision != 2 || selected.Selection == nil || selected.Selection.TaskRunID != first || selected.AllTerminal {
		t.Fatalf("selection: %+v", selected)
	}
	// Selection neither overwrites the result nor cancels the remaining worker.
	b, _ = s.TaskRun(ctx, f.SeatID, second)
	if b.State != runtimeapi.TaskPending {
		t.Fatalf("sibling changed to %s", b.State)
	}
	a, _ = s.TaskRun(ctx, f.SeatID, first)
	if a.Result.Artifacts[0].Content != output {
		t.Fatal("selected artifact mutated")
	}
	if _, err := s.SelectRunBranch(ctx, f, choice); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	choice.ExpectedRevision = 2
	if selected, err = s.SelectRunBranch(ctx, f, choice); err != nil || selected.Revision != 2 {
		t.Fatalf("same choice: %+v %v", selected, err)
	}
	completeAlternative(t, s, f.SeatID, second, "alternative result")
	group, _ = s.CompareRunBranch(ctx, f.SeatID, group.ID)
	choice.TaskRunID, choice.ExpectedResultSHA256 = second, group.Alternatives[1].ResultSHA256
	selected, err = s.SelectRunBranch(ctx, f, choice)
	if err != nil || selected.Revision != 3 || !selected.AllTerminal {
		t.Fatalf("replacement: %+v %v", selected, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM run_branch_selections WHERE branch_id=$1`, group.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("selection history %d %v", count, err)
	}
}

func TestRunBranchIdempotencyFencingAndAtomicAdmission(t *testing.T) {
	ctx := context.Background()
	s, f, execution, stranger := branchFixture(t)
	req := branchRequest()
	var groups [2]*runtimeapi.RunBranchComparison
	var errs [2]error
	var wg sync.WaitGroup
	for i := range groups {
		wg.Add(1)
		go func(i int) { defer wg.Done(); groups[i], errs[i] = s.CreateRunBranch(ctx, f, execution, req) }(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || groups[0].ID != groups[1].ID {
		t.Fatalf("concurrent retry: %+v %+v", groups, errs)
	}
	changed := branchRequest()
	changed.RequestID = req.RequestID
	changed.Handoff = "changed objective"
	if _, err := s.CreateRunBranch(ctx, f, execution, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	stale := f
	stale.Generation--
	if _, err := s.CreateRunBranch(ctx, stale, execution, req); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale retry: %v", err)
	}
	// Three active children leave only one slot. The two-child group must
	// roll back both its first insertion and its group record.
	if _, err := s.CreateTaskRun(ctx, f, execution, runtimeapi.TaskRunRequest{RequestID: uuid.NewString(), Prompt: "occupy one slot"}); err != nil {
		t.Fatal(err)
	}
	failed := branchRequest()
	if _, err := s.CreateRunBranch(ctx, f, execution, failed); !errors.Is(err, ErrConflict) {
		t.Fatalf("over capacity: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_runs WHERE parent_seat_id=$1`, f.SeatID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("partial children retained: %d %v", count, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM run_branches WHERE request_id=$1`, failed.RequestID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial group retained: %d %v", count, err)
	}
	otherFence := lease(t, s, stranger)
	if _, err := s.CreateRunBranch(ctx, otherFence, execution, branchRequest()); !errors.Is(err, ErrFenced) {
		t.Fatalf("foreign parent execution: %v", err)
	}
	selectReq := runtimeapi.RunBranchSelectRequest{ID: groups[0].ID, TaskRunID: groups[0].Alternatives[0].TaskRunID, ExpectedRevision: 1, ExpectedResultSHA256: strings.Repeat("0", 64)}
	if _, err := s.SelectRunBranch(ctx, otherFence, selectReq); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign selection: %v", err)
	}
}

func TestRunBranchConcurrentChoiceAndMembership(t *testing.T) {
	ctx := context.Background()
	s, f, execution, _ := branchFixture(t)
	a, err := s.CreateRunBranch(ctx, f, execution, branchRequest())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRunBranch(ctx, f, execution, branchRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []*runtimeapi.RunBranchComparison{a, b} {
		for _, alt := range group.Alternatives {
			completeAlternative(t, s, f.SeatID, alt.TaskRunID, alt.Name)
		}
	}
	a, _ = s.CompareRunBranch(ctx, f.SeatID, a.ID)
	b, _ = s.CompareRunBranch(ctx, f.SeatID, b.ID)
	wrong := runtimeapi.RunBranchSelectRequest{ID: a.ID, TaskRunID: b.Alternatives[0].TaskRunID, ExpectedRevision: 1, ExpectedResultSHA256: b.Alternatives[0].ResultSHA256}
	if _, err := s.SelectRunBranch(ctx, f, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group result accepted: %v", err)
	}
	var errs [2]error
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.SelectRunBranch(ctx, f, runtimeapi.RunBranchSelectRequest{ID: a.ID, TaskRunID: a.Alternatives[i].TaskRunID, ExpectedRevision: 1, ExpectedResultSHA256: a.Alternatives[i].ResultSHA256})
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent selection: %v", errs)
	}
}

func TestRunBranchRejectsFailedCancelledAndTimedOutResults(t *testing.T) {
	for _, state := range []string{runtimeapi.TaskFailed, runtimeapi.TaskCancelled, runtimeapi.TaskTimedOut} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, f, execution, _ := branchFixture(t)
			group, err := s.CreateRunBranch(ctx, f, execution, branchRequest())
			if err != nil {
				t.Fatal(err)
			}
			id := group.Alternatives[0].TaskRunID
			if state == runtimeapi.TaskCancelled {
				err = s.CancelTaskRun(ctx, f, id)
			} else {
				if _, err = s.ClaimTaskRun(ctx, f.SeatID, id, "pod"); err != nil {
					t.Fatal(err)
				}
				err = s.FinishTaskRun(ctx, f.SeatID, id, "pod", runtimeapi.TaskRunFinish{State: state, Result: runtimeapi.TaskRunResult{Output: "partial output", Error: "did not finish"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			group, err = s.CompareRunBranch(ctx, f.SeatID, group.ID)
			if err != nil {
				t.Fatal(err)
			}
			digest := group.Alternatives[0].ResultSHA256
			if digest == "" {
				digest = strings.Repeat("0", 64)
			}
			if _, err = s.SelectRunBranch(ctx, f, runtimeapi.RunBranchSelectRequest{ID: group.ID, TaskRunID: id, ExpectedRevision: 1, ExpectedResultSHA256: digest}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s chosen: %v", state, err)
			}
		})
	}
}

func TestRunBranchPurgePreservesOrganizationDeletion(t *testing.T) {
	ctx := context.Background()
	s, f, execution, _ := branchFixture(t)
	group, err := s.CreateRunBranch(ctx, f, execution, branchRequest())
	if err != nil {
		t.Fatal(err)
	}
	completeAlternative(t, s, f.SeatID, group.Alternatives[0].TaskRunID, "chosen")
	group, err = s.CompareRunBranch(ctx, f.SeatID, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SelectRunBranch(ctx, f, runtimeapi.RunBranchSelectRequest{ID: group.ID, TaskRunID: group.Alternatives[0].TaskRunID, ExpectedRevision: 1, ExpectedResultSHA256: group.Alternatives[0].ResultSHA256}); err != nil {
		t.Fatal(err)
	}
	seat, err := s.Seat(ctx, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOrganization(ctx, seat.OrganizationID, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"run_branches", "run_branch_members", "run_branch_selections"} {
		var count int
		if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained %d rows: %v", table, count, err)
		}
	}
}
