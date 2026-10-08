// Package scheduler runs the platform's durable background work: running
// due automations (§9.3), returning expired delivery leases to the queue,
// recording interrupted connector attempts as unknown, and refreshing the
// operational gauges (§14).
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

// Store is the persistence the loop needs.
type Store interface {
	FireDue(ctx context.Context, now time.Time, limit int) (int, error)
	RequeueExpired(ctx context.Context) (int, error)
	ExpireStaleOperations(ctx context.Context, olderThan time.Duration) (int64, error)
	Gauges(ctx context.Context) (*store.Gauges, error)
}

// StaleOperation is how long an operation may stay pending before its
// attempt is presumed interrupted.
const StaleOperation = 15 * time.Minute

// Loop runs the background work every Interval.
type Loop struct {
	Store    Store
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	Interval time.Duration
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

// Tick performs one round of background work.
func (l *Loop) Tick(ctx context.Context) {
	report := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			l.Log.Error(what, "error", err)
		}
	}
	for {
		n, err := l.Store.FireDue(ctx, time.Now(), 100)
		report("run due automations", err)
		l.Metrics.AutomationRuns.Add(float64(n))
		if err != nil || n < 100 {
			break
		}
	}
	dead, err := l.Store.RequeueExpired(ctx)
	report("requeue expired deliveries", err)
	if dead > 0 {
		l.Metrics.DeadLettered.Add(float64(dead))
		l.Log.Warn("deliveries dead-lettered", "count", dead)
	}
	n, err := l.Store.ExpireStaleOperations(ctx, StaleOperation)
	report("expire stale operations", err)
	if n > 0 {
		l.Log.Warn("interrupted connector operations recorded as unknown", "count", n)
	}
	g, err := l.Store.Gauges(ctx)
	report("compute gauges", err)
	if err == nil {
		l.Metrics.Seats.Reset()
		for k, v := range g.SeatsByState {
			l.Metrics.Seats.WithLabelValues(k[0], k[1]).Set(float64(v))
		}
		l.Metrics.QueueAge.Reset()
		for org, age := range g.OldestPending {
			l.Metrics.QueueAge.WithLabelValues(org).Set(age)
		}
	}
}
