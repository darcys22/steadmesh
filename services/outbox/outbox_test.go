package outbox_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/outbox"
	"github.com/darcys22/steadmesh/services/store"
)

type resolved struct {
	state string
	wait  time.Duration
}

type fakeStore struct {
	org      *store.Organization
	pending  []store.OutboxItem
	resolved []resolved
}

func (f *fakeStore) ClaimOutbox(context.Context, int) ([]store.OutboxItem, error) {
	out := f.pending
	f.pending = nil
	return out, nil
}

func (f *fakeStore) ResolveOutbox(_ context.Context, _ store.OutboxItem, state, _, _ string, wait time.Duration) error {
	f.resolved = append(f.resolved, resolved{state, wait})
	return nil
}

func (f *fakeStore) ExpireSending(context.Context) (int, error) { return 0, nil }

func (f *fakeStore) Organization(context.Context, string) (*store.Organization, error) {
	return f.org, nil
}

type comms struct{ c connectors.Communication }

func (c comms) Communication(string, string) (connectors.Communication, error) { return c.c, nil }

func TestSendOutcomes(t *testing.T) {
	cases := map[string]struct {
		err      error
		attempts int
		want     resolved
	}{
		"sent":                    {nil, 1, resolved{store.OutboxSent, 0}},
		"ambiguous is unknown":    {fmt.Errorf("timeout: %w", connectors.ErrAmbiguous), 1, resolved{store.OutboxUnknown, 0}},
		"unclassified is unknown": {fmt.Errorf("connection reset"), 1, resolved{store.OutboxUnknown, 0}},
		"retryable backs off":     {fmt.Errorf("429: %w", connectors.ErrRetryable), 2, resolved{store.OutboxPending, 8 * time.Second}},
		"retryable exhausted":     {fmt.Errorf("429: %w", connectors.ErrRetryable), store.MaxAttempts, resolved{store.OutboxDead, 0}},
		"permanent":               {fmt.Errorf("channel_not_found: %w", connectors.ErrPermanent), 1, resolved{store.OutboxDead, 0}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			comm := fakeconn.NewComm()
			comm.SendErrs = []error{tc.err}
			fs := &fakeStore{org: &store.Organization{Manifest: *orgfixture.Manifest(t)}, pending: []store.OutboxItem{{
				ID: 1, OrganizationID: "org", MessageID: "m", OperationID: "0123456789", Connection: "slack",
				Binding: "alice", ExternalRef: "D1#171.2", Body: "hi", Attempts: tc.attempts,
			}}}
			d := &outbox.Dispatcher{Store: fs, Comms: comms{comm}, Metrics: metrics.NewUnregistered(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if _, err := d.DispatchOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(fs.resolved) != 1 || fs.resolved[0] != tc.want {
				t.Fatalf("resolved = %+v, want %+v", fs.resolved, tc.want)
			}
			sent := comm.Sent()
			if len(sent) != 1 || sent[0].ExternalUserID != orgfixture.UserA || sent[0].ChannelID != "D1" || sent[0].ThreadRef != "171.2" ||
				sent[0].IdempotencyKey != "0123456789" {
				t.Fatalf("sent = %+v", sent)
			}
		})
	}
}
