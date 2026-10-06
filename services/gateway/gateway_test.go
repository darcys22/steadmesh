package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

type ledger struct {
	mu  sync.Mutex
	ops map[string]*runtimeapi.Operation
}

func (l *ledger) BeginOperation(_ context.Context, _ store.Fence, in store.NewOperation) (*runtimeapi.Operation, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ops == nil {
		l.ops = map[string]*runtimeapi.Operation{}
	}
	k := in.Connection + "/" + in.IdempotencyKey
	if op, ok := l.ops[k]; ok {
		c := *op
		return &c, false, nil
	}
	op := &runtimeapi.Operation{ID: uuid.NewString(), Connection: in.Connection, Operation: in.Operation, Target: in.Target,
		IdempotencyKey: in.IdempotencyKey, Status: store.OpPending}
	l.ops[k] = op
	c := *op
	return &c, true, nil
}

func (l *ledger) FinishOperation(_ context.Context, id, status, receipt string, result json.RawMessage, errText string, _ int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, op := range l.ops {
		if op.ID == id {
			op.Status, op.ExternalReceipt, op.Result, op.Error = status, receipt, result, errText
			return nil
		}
	}
	return errors.New("no such operation")
}

type trackers struct{ t connectors.Tracker }

func (t trackers) Tracker(string, string) (connectors.Tracker, error) { return t.t, nil }

func setup(t *testing.T, tr *fakeconn.Tracker, seatKey string) (*gateway.Gateway, *store.Seat) {
	t.Helper()
	g := &gateway.Gateway{Ledger: &ledger{}, Trackers: trackers{tr}, Metrics: metrics.NewUnregistered(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Attempts: 3}
	return g, &store.Seat{ID: uuid.NewString(), OrganizationID: "org", Key: seatKey, Manifest: orgfixture.Manifest(t).Seats[seatKey]}
}

func invoke(g *gateway.Gateway, seat *store.Seat, op, params string) (*runtimeapi.Operation, error) {
	return g.Invoke(context.Background(), gateway.Request{Seat: seat, Generation: 1, Connection: "tracker", Operation: op, Params: json.RawMessage(params)})
}

func TestSuccessAndIdempotentReplay(t *testing.T) {
	tr := &fakeconn.Tracker{}
	g, seat := setup(t, tr, "lead")
	op, err := invoke(g, seat, "project.create", `{"name":"Apollo","team_id":"T1"}`)
	if err != nil || op.Status != store.OpSucceeded || op.ExternalReceipt == "" || op.Target != "T1" {
		t.Fatalf("op = %+v, %v", op, err)
	}
	// The same request with keys in a different order is the same operation.
	again, err := invoke(g, seat, "project.create", `{"team_id":"T1","name":"Apollo"}`)
	if err != nil || again.ID != op.ID || again.Status != store.OpSucceeded {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(tr.Calls()); n != 1 {
		t.Fatalf("tracker invoked %d times, want 1", n)
	}
}

func TestAmbiguousOutcome(t *testing.T) {
	lost := fmt.Errorf("read timeout: %w", connectors.ErrAmbiguous)
	t.Run("not landed is unknown and never replayed", func(t *testing.T) {
		tr := &fakeconn.Tracker{Script: []error{lost}}
		g, seat := setup(t, tr, "lead")
		op, err := invoke(g, seat, "project.create", `{"name":"Apollo"}`)
		if err != nil || op.Status != store.OpUnknown {
			t.Fatalf("op = %+v, %v", op, err)
		}
		again, err := invoke(g, seat, "project.create", `{"name":"Apollo"}`)
		if err != nil || again.ID != op.ID || again.Status != store.OpUnknown {
			t.Fatalf("replay = %+v, %v", again, err)
		}
		if n := len(tr.Calls()); n != 1 {
			t.Fatalf("unknown operation re-executed: %d calls", n)
		}
	})
	t.Run("landed is resolved by read-back", func(t *testing.T) {
		tr := &fakeconn.Tracker{Script: []error{lost}, Landed: true}
		g, seat := setup(t, tr, "lead")
		op, err := invoke(g, seat, "project.create", `{"name":"Apollo"}`)
		if err != nil || op.Status != store.OpSucceeded || op.ExternalReceipt == "" {
			t.Fatalf("op = %+v, %v", op, err)
		}
		if tr.Applied() != 1 || len(tr.Calls()) != 1 {
			t.Fatalf("applied=%d calls=%d", tr.Applied(), len(tr.Calls()))
		}
	})
	t.Run("unclassified errors are treated as ambiguous", func(t *testing.T) {
		tr := &fakeconn.Tracker{Script: []error{errors.New("connection reset")}}
		g, seat := setup(t, tr, "lead")
		if op, _ := invoke(g, seat, "task.write", `{"title":"x"}`); op.Status != store.OpUnknown {
			t.Fatalf("status = %s", op.Status)
		}
	})
}

func TestRetryablePermanentAndReads(t *testing.T) {
	retry := fmt.Errorf("429: %w", connectors.ErrRetryable)
	tr := &fakeconn.Tracker{Script: []error{retry, retry}}
	g, seat := setup(t, tr, "lead")
	if op, _ := invoke(g, seat, "project.create", `{"name":"a"}`); op.Status != store.OpSucceeded || len(tr.Calls()) != 3 {
		t.Fatalf("status=%s calls=%d", op.Status, len(tr.Calls()))
	}

	tr = &fakeconn.Tracker{Script: []error{retry, retry, retry}}
	g, seat = setup(t, tr, "lead")
	if op, _ := invoke(g, seat, "project.create", `{"name":"a"}`); op.Status != store.OpFailed || len(tr.Calls()) != 3 {
		t.Fatalf("exhausted retries: status=%s calls=%d", op.Status, len(tr.Calls()))
	}

	tr = &fakeconn.Tracker{Script: []error{fmt.Errorf("bad input: %w", connectors.ErrPermanent)}}
	g, seat = setup(t, tr, "lead")
	if op, _ := invoke(g, seat, "project.create", `{"name":"a"}`); op.Status != store.OpFailed || len(tr.Calls()) != 1 {
		t.Fatalf("permanent: status=%s calls=%d", op.Status, len(tr.Calls()))
	}

	tr = &fakeconn.Tracker{}
	g, seat = setup(t, tr, "lead")
	a, _ := invoke(g, seat, "project.read", `{"id":"p"}`)
	b, _ := invoke(g, seat, "project.read", `{"id":"p"}`)
	if a.ID == b.ID || len(tr.Calls()) != 2 {
		t.Fatal("reads must not be deduplicated into a stale result")
	}
}

func TestGrantEnforced(t *testing.T) {
	tr := &fakeconn.Tracker{}
	g, seat := setup(t, tr, "engineer")
	if _, err := invoke(g, seat, "project.create", `{}`); !errors.Is(err, gateway.ErrForbidden) {
		t.Fatalf("seat without grant: %v", err)
	}
	g, seat = setup(t, tr, "lead")
	if _, err := invoke(g, seat, "comment.write", `{}`); !errors.Is(err, gateway.ErrForbidden) {
		t.Fatalf("ungranted operation: %v", err)
	}
	if _, err := invoke(g, seat, "project.create", `[1]`); !errors.Is(err, gateway.ErrInvalid) {
		t.Fatalf("non-object params: %v", err)
	}
	if len(tr.Calls()) != 0 {
		t.Fatal("denied call reached the tracker")
	}
}
