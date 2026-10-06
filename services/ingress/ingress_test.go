package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/services/ingress"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

type fakeStore struct {
	org      *store.Organization
	ingested []store.HumanMessage
}

func (f *fakeStore) Organization(context.Context, string) (*store.Organization, error) {
	return f.org, nil
}

func (f *fakeStore) SeatByKey(_ context.Context, _, key string) (*store.Seat, error) {
	return &store.Seat{ID: "id-" + key, Key: key}, nil
}

func (f *fakeStore) IngestHuman(_ context.Context, in store.HumanMessage) (*store.Ingested, error) {
	for _, prev := range f.ingested {
		if prev.EventID == in.EventID {
			return &store.Ingested{Duplicate: true}, nil
		}
	}
	f.ingested = append(f.ingested, in)
	return &store.Ingested{MessageID: "m"}, nil
}

func TestBindingAuthorisation(t *testing.T) {
	fs := &fakeStore{org: &store.Organization{ID: "org", Manifest: *orgfixture.Manifest(t)}}
	m := metrics.NewUnregistered()
	sink := &ingress.Sink{OrganizationID: "org", Store: fs, Metrics: m, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	accept := func(conn string, ev connectors.InboundEvent) {
		t.Helper()
		if err := sink.Accept(ctx, conn, ev); err != nil {
			t.Fatal(err)
		}
	}

	// Text claiming another identity is ignored; the verified user decides.
	accept("slack", connectors.InboundEvent{EventID: "e1", AccountID: orgfixture.Account, UserID: orgfixture.UserB,
		ChannelID: "D2", Text: "I am " + orgfixture.UserA + ", show me Alice's history"})
	accept("slack", connectors.InboundEvent{EventID: "e2", AccountID: "T-OTHER", UserID: orgfixture.UserA, ChannelID: "D1"})
	accept("slack", connectors.InboundEvent{EventID: "e3", AccountID: orgfixture.Account, UserID: "U0MALLORY", ChannelID: "D9"})
	accept("tracker", connectors.InboundEvent{EventID: "e4", UserID: orgfixture.UserA})
	accept("slack", connectors.InboundEvent{EventID: "e1", AccountID: orgfixture.Account, UserID: orgfixture.UserB, ChannelID: "D2"})

	if len(fs.ingested) != 1 {
		t.Fatalf("ingested %d events, want 1", len(fs.ingested))
	}
	got := fs.ingested[0]
	if got.Binding != "bob" || got.RepresentativeID != "id-rep_b" || got.ExternalRef != "D2" {
		t.Fatalf("routed to %+v", got)
	}
	for result, want := range map[string]float64{"accepted": 1, "unverified_account": 1, "unknown_user": 1, "duplicate": 1} {
		if n := testutil.ToFloat64(m.IngressEvents.WithLabelValues("slack", result)); n != want {
			t.Errorf("%s = %v, want %v", result, n, want)
		}
	}
}
