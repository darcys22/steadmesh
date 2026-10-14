//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func TestOperationReplayBindsAuthorityAndRequest(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org := syncOrg(t, s, orgfixture.Manifest(t))
	f := lease(t, s, org.Seats["lead"].SeatID)
	other := lease(t, s, org.Seats["engineer"].SeatID)
	in := NewOperation{OrganizationID: org.OrganizationID, SeatID: f.SeatID, ExecutionID: uuid.NewString(),
		Connection: "tracker", Operation: "project.read", Target: "private-project", RequestHash: "private-request", IdempotencyKey: "known-key"}
	op, created, err := s.BeginOperation(ctx, f, in)
	if err != nil || !created || op == nil {
		t.Fatalf("begin: op=%+v created=%v err=%v", op, created, err)
	}
	private := json.RawMessage(`{"private":"recorded result"}`)
	if err := s.FinishOperation(ctx, op.ID, OpSucceeded, "private-receipt", private, "", 1); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		fence  Fence
		change func(*NewOperation)
	}{
		{"different stable seat", other, func(in *NewOperation) { in.SeatID = other.SeatID }},
		{"different operation", f, func(in *NewOperation) { in.Operation = "project.create" }},
		{"different resolved target", f, func(in *NewOperation) { in.Target = "allowed-project" }},
		{"different request hash", f, func(in *NewOperation) { in.RequestHash = "different-request" }},
		{"allowed request from another seat", other, func(in *NewOperation) {
			in.SeatID, in.Target, in.RequestHash = other.SeatID, "allowed-project", "allowed-request"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := in
			tc.change(&changed)
			got, created, err := s.BeginOperation(ctx, tc.fence, changed)
			if !errors.Is(err, ErrConflict) || got != nil || created {
				t.Fatalf("mismatched replay disclosed or created operation: op=%+v created=%v err=%v", got, created, err)
			}
		})
	}

	// A retry belongs to the stable seat, not its execution or lease generation.
	if _, err := s.FenceSeat(ctx, in.OrganizationID, f.SeatID, f.Generation); err != nil {
		t.Fatal(err)
	}
	next := lease(t, s, f.SeatID)
	in.ExecutionID = uuid.NewString()
	again, created, err := s.BeginOperation(ctx, next, in)
	if err != nil || created || again == nil || again.ID != op.ID || again.Status != OpSucceeded || again.ExternalReceipt != "private-receipt" {
		t.Fatalf("identical retry: op=%+v created=%v err=%v", again, created, err)
	}
	var result map[string]string
	if err := json.Unmarshal(again.Result, &result); err != nil || result["private"] != "recorded result" {
		t.Fatalf("retry lost recorded result: result=%s err=%v", again.Result, err)
	}
	if got, created, err := s.BeginOperation(ctx, f, in); !errors.Is(err, ErrFenced) || got != nil || created {
		t.Fatalf("stale retry: op=%+v created=%v err=%v", got, created, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM connector_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replays changed ledger cardinality: count=%d err=%v", count, err)
	}
}

func TestOperationReplayPreservesRecordedStatus(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org := syncOrg(t, s, orgfixture.Manifest(t))
	f := lease(t, s, org.Seats["lead"].SeatID)
	for _, status := range []string{OpPending, OpSucceeded, OpFailed, OpUnknown} {
		t.Run(status, func(t *testing.T) {
			in := NewOperation{OrganizationID: org.OrganizationID, SeatID: f.SeatID, Connection: "tracker",
				Operation: "task.write", Target: "task-1", RequestHash: "same-request", IdempotencyKey: status}
			op, created, err := s.BeginOperation(ctx, f, in)
			if err != nil || !created {
				t.Fatalf("begin: op=%+v created=%v err=%v", op, created, err)
			}
			if status != OpPending {
				if err := s.FinishOperation(ctx, op.ID, status, "receipt", json.RawMessage(`{"done":true}`), "recorded error", 1); err != nil {
					t.Fatal(err)
				}
			}
			again, created, err := s.BeginOperation(ctx, f, in)
			if err != nil || created || again == nil || again.ID != op.ID || again.Status != status {
				t.Fatalf("retry: op=%+v created=%v err=%v", again, created, err)
			}
		})
	}
}

func TestOperationReplayCannotReadSystemOperation(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org := syncOrg(t, s, orgfixture.Manifest(t))
	f := lease(t, s, org.Seats["lead"].SeatID)
	in := NewOperation{OrganizationID: org.OrganizationID, Connection: "tracker", Operation: "project.read",
		Target: "private-project", RequestHash: "same-request", IdempotencyKey: "system-key"}
	op, created, err := s.BeginSystemOperation(ctx, in)
	if err != nil || !created {
		t.Fatalf("system begin: op=%+v created=%v err=%v", op, created, err)
	}
	in.SeatID = f.SeatID
	got, created, err := s.BeginOperation(ctx, f, in)
	if !errors.Is(err, ErrConflict) || got != nil || created {
		t.Fatalf("seat replayed system operation: op=%+v created=%v err=%v", got, created, err)
	}
}

func TestConcurrentOperationKeyReuseCannotCrossSeats(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org := syncOrg(t, s, orgfixture.Manifest(t))
	fences := []Fence{lease(t, s, org.Seats["lead"].SeatID), lease(t, s, org.Seats["engineer"].SeatID)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	created := make([]bool, len(fences))
	errs := make([]error, len(fences))
	for i, f := range fences {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			in := NewOperation{OrganizationID: org.OrganizationID, SeatID: f.SeatID, Connection: "tracker",
				Operation: "project.read", Target: "project-1", RequestHash: "same-request", IdempotencyKey: "shared-key"}
			op, wasCreated, err := s.BeginOperation(ctx, f, in)
			created[i], errs[i] = wasCreated, err
			if err != nil && op != nil {
				t.Errorf("rejected concurrent replay disclosed operation: %+v", op)
			}
		}()
	}
	close(start)
	wg.Wait()
	var successes, conflicts int
	for i, err := range errs {
		switch {
		case err == nil && created[i]:
			successes++
		case errors.Is(err, ErrConflict) && !created[i]:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent outcome: created=%v err=%v", created[i], err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("created=%d conflicts=%d, want one of each", successes, conflicts)
	}
}
