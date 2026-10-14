//go:build integration

package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/linear"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestGatewayReplayCannotDiscloseAnotherAuthoritysResult(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	org, err := s.SyncOrganization(ctx, store.SyncInput{Namespace: "acme", Key: "acme", Manifest: orgfixture.Manifest(t)})
	if err != nil {
		t.Fatal(err)
	}
	request := func(key, target string) gateway.Request {
		t.Helper()
		seat, err := s.Seat(ctx, org.Seats[key].SeatID)
		if err != nil {
			t.Fatal(err)
		}
		grantTargets(seat, "project.read", []string{target})
		lease, err := s.AcquireLease(ctx, seat.ID, "pod-"+key)
		if err != nil {
			t.Fatal(err)
		}
		return gateway.Request{Seat: seat, Generation: lease.Generation, Connection: "tracker", Operation: "project.read",
			Params: json.RawMessage(`{"id":"` + target + `"}`), IdempotencyKey: "known-key"}
	}
	private := request("lead", "private-project")
	restricted := request("engineer", "allowed-project")
	fake := &fakeconn.Tracker{}
	adapter, err := linear.New(connectors.Config{Secret: map[string]string{"api_key": "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := setup(t, fake, "lead")
	g.Ledger = s
	g.Trackers = trackers{&boundTracker{Tracker: fake, resolver: adapter.(connectors.TargetResolver)}}
	first, err := g.Invoke(ctx, private)
	if err != nil || first == nil || first.Status != store.OpSucceeded {
		t.Fatalf("private operation: op=%+v err=%v", first, err)
	}
	// Store recognizable private content so a leaked replay is unambiguous.
	if err := s.FinishOperation(ctx, first.ID, store.OpSucceeded, "private-receipt", json.RawMessage(`{"private":"secret"}`), "", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := g.Invoke(ctx, restricted); !errors.Is(err, store.ErrConflict) || got != nil {
		t.Fatalf("allowed target replay disclosed denied target: op=%+v err=%v", got, err)
	}
	if len(fake.Calls()) != 1 {
		t.Fatalf("conflicting replay reached connector: calls=%v", fake.Calls())
	}

	// Also bind the seat even if both callers can access the same target.
	sameTarget := restricted
	sameTarget.Params = private.Params
	grantTargets(sameTarget.Seat, "project.read", []string{"private-project"})
	if got, err := g.Invoke(ctx, sameTarget); !errors.Is(err, store.ErrConflict) || got != nil {
		t.Fatalf("another seat replayed same request: op=%+v err=%v", got, err)
	}

	again, err := g.Invoke(ctx, private)
	if err != nil || again == nil || again.ID != first.ID || again.ExternalReceipt != "private-receipt" || len(fake.Calls()) != 1 {
		t.Fatalf("legitimate replay changed: op=%+v err=%v calls=%v", again, err, fake.Calls())
	}

	// Changed permissions cannot be bypassed by replaying an earlier result.
	grantTargets(private.Seat, "project.read", []string{"allowed-project"})
	if got, err := g.Invoke(ctx, private); !errors.Is(err, gateway.ErrForbidden) || got != nil {
		t.Fatalf("revoked target replay allowed: op=%+v err=%v", got, err)
	}
}

func TestGatewayReplayRejectsChangedResolvedDefault(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	org, err := s.SyncOrganization(ctx, store.SyncInput{Namespace: "acme", Key: "acme", Manifest: orgfixture.Manifest(t)})
	if err != nil {
		t.Fatal(err)
	}
	seat, err := s.Seat(ctx, org.Seats["lead"].SeatID)
	if err != nil {
		t.Fatal(err)
	}
	seat.Manifest.Capabilities = []compile.Capability{{Resource: "connection:tracker", Operations: []string{"project.create"}, Targets: []string{"team-*"}}}
	lease, err := s.AcquireLease(ctx, seat.ID, "pod-lead")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeconn.Tracker{}
	g, _ := setup(t, fake, "lead")
	g.Ledger = s
	withDefault := func(team string) {
		t.Helper()
		adapter, err := linear.New(connectors.Config{Secret: map[string]string{"api_key": "fixture"}, Extra: map[string]string{"team_id": team}})
		if err != nil {
			t.Fatal(err)
		}
		g.Trackers = trackers{&boundTracker{Tracker: fake, resolver: adapter.(connectors.TargetResolver)}}
	}
	req := gateway.Request{Seat: seat, Generation: lease.Generation, Connection: "tracker", Operation: "project.create",
		Params: json.RawMessage(`{"name":"New"}`), IdempotencyKey: "same-input"}
	withDefault("team-one")
	first, err := g.Invoke(ctx, req)
	if err != nil || first == nil || first.Status != store.OpSucceeded || first.Target != "team-one" {
		t.Fatalf("first: op=%+v err=%v", first, err)
	}
	// The request hash is unchanged, but configuration resolves a new target.
	withDefault("team-two")
	if got, err := g.Invoke(ctx, req); !errors.Is(err, store.ErrConflict) || got != nil || len(fake.Calls()) != 1 {
		t.Fatalf("changed default replay: op=%+v err=%v calls=%v", got, err, fake.Calls())
	}
}
