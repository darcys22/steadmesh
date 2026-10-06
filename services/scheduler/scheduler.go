// Package scheduler runs the platform's durable background work: firing wake
// schedules (§9.3), returning expired delivery leases to the queue,
// recording interrupted connector attempts as unknown, and refreshing the
// operational gauges (§14).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

// MinInterval is the shortest recurring wake interval.
const MinInterval = time.Minute

const everyPrefix = "@every "

// Parse validates a wake request: either a one-off RFC 3339 time or a
// recurring interval. It returns the stored schedule text and first trigger.
func Parse(at, every string, now time.Time) (string, time.Time, error) {
	switch {
	case at != "" && every != "":
		return "", time.Time{}, errors.New("give either at or every, not both")
	case at != "":
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("at must be an RFC 3339 time: %w", err)
		}
		return t.UTC().Format(time.RFC3339), t, nil
	case every != "":
		d, err := time.ParseDuration(every)
		if err != nil || d < MinInterval {
			return "", time.Time{}, fmt.Errorf("every must be a duration of at least %s", MinInterval)
		}
		return everyPrefix + d.String(), now.Add(d), nil
	default:
		return "", time.Time{}, errors.New("give at or every")
	}
}

// Next is the store.NextFunc for schedules produced by Parse. Missed
// intervals are skipped rather than fired in a burst.
func Next(schedule string, fired, now time.Time) (time.Time, bool) {
	rest, ok := strings.CutPrefix(schedule, everyPrefix)
	if !ok {
		return time.Time{}, false
	}
	d, err := time.ParseDuration(rest)
	if err != nil || d < MinInterval {
		return time.Time{}, false
	}
	next := fired.Add(d)
	if !next.After(now) {
		next = next.Add(now.Sub(next).Truncate(d) + d)
	}
	return next, true
}

// Store is the persistence the loop needs.
type Store interface {
	FireDue(ctx context.Context, now time.Time, limit int, next store.NextFunc) (int, error)
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
		n, err := l.Store.FireDue(ctx, time.Now(), 100, Next)
		report("fire wake schedules", err)
		l.Metrics.SchedulesFired.Add(float64(n))
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
