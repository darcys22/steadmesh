package scheduler_test

import (
	"testing"
	"time"

	"github.com/darcys22/steadmesh/services/scheduler"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, next, err := scheduler.Parse("2026-10-06T09:00:00+01:00", "", now)
	if err != nil || s != "2026-10-06T08:00:00Z" || !next.Equal(time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("one-off: %q %v %v", s, next, err)
	}
	s, next, err = scheduler.Parse("", "90m", now)
	if err != nil || s != "@every 1h30m0s" || !next.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("interval: %q %v %v", s, next, err)
	}
	for _, bad := range [][2]string{{"", ""}, {"tomorrow", ""}, {"", "30s"}, {"2026-10-06T09:00:00Z", "1h"}} {
		if _, _, err := scheduler.Parse(bad[0], bad[1], now); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestNext(t *testing.T) {
	fired := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if _, ok := scheduler.Next("2026-10-05T12:00:00Z", fired, fired); ok {
		t.Fatal("one-off schedule recurred")
	}
	next, ok := scheduler.Next("@every 1h0m0s", fired, fired.Add(time.Minute))
	if !ok || !next.Equal(fired.Add(time.Hour)) {
		t.Fatalf("next = %v", next)
	}
	// After downtime, missed intervals are skipped.
	next, _ = scheduler.Next("@every 1h0m0s", fired, fired.Add(5*time.Hour+10*time.Minute))
	if !next.Equal(fired.Add(6 * time.Hour)) {
		t.Fatalf("next after downtime = %v", next)
	}
}
