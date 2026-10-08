// Package retirement completes graceful seat retirement (§5.5). Platform sync
// marks a seat removed from the declaration as retiring and gives it a
// retirement notice turn; this loop retires it once that turn is done or its
// grace period ends, handing back what it still held and revoking the
// credentials delivered to its sandbox.
package retirement

import (
	"context"
	"log/slog"
	"time"

	"github.com/darcys22/steadmesh/services/credentials"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
)

// Store is the persistence the loop needs.
type Store interface {
	DueRetirements(ctx context.Context, limit int) ([]store.DueRetirement, error)
	FinishRetirement(ctx context.Context, seatID string, release store.WorkRelease) (*store.Retirement, error)
}

// Revoker revokes the credentials delivered to a seat.
type Revoker interface {
	RevokeSeat(ctx context.Context, org, seat string) []credentials.Revoked
}

// Loop retires due seats every Interval.
type Loop struct {
	Store       Store
	Credentials Revoker
	Metrics     *metrics.Metrics
	Log         *slog.Logger
	Interval    time.Duration
}

// Run loops until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	for {
		l.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick retires every due seat.
func (l *Loop) Tick(ctx context.Context) {
	due, err := l.Store.DueRetirements(ctx, 50)
	if err != nil {
		if ctx.Err() == nil {
			l.Log.Error("list due retirements", "error", err)
		}
		return
	}
	for _, d := range due {
		r, err := l.Store.FinishRetirement(ctx, d.SeatID, tools.ReleaseRetired(d.Key))
		if err != nil {
			if ctx.Err() == nil {
				l.Log.Error("retire seat", "organization_id", d.OrganizationID, "seat_id", d.SeatID, "seat", d.Key, "error", err)
			}
			continue
		}
		if r == nil {
			continue
		}
		if l.Credentials != nil {
			l.Credentials.RevokeSeat(context.WithoutCancel(ctx), r.OrganizationID, r.Key)
		}
		outcome := "finished"
		if r.Overdue {
			outcome = "overdue"
		}
		if l.Metrics != nil {
			l.Metrics.SeatsRetired.WithLabelValues(outcome).Inc()
		}
		l.Log.Info("seat retired", "organization_id", r.OrganizationID, "seat_id", r.SeatID, "seat", r.Key, "outcome", outcome,
			"interrupted", r.Interrupted, "returned_messages", r.Returned, "released_work", r.ReleasedWork, "notified", r.Notified)
		if len(r.Notified) == 0 {
			l.Log.Warn("no representative to tell about a retired seat", "organization_id", r.OrganizationID, "seat", r.Key)
		}
	}
}
