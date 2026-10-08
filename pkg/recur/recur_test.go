package recur_test

import (
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/recur"
)

var melbourne, _ = time.LoadLocation("Australia/Melbourne")

func mustParse(t *testing.T, req recur.Request, now time.Time) recur.Rule {
	t.Helper()
	r, err := recur.Parse(req, "Australia/Melbourne", now)
	if err != nil {
		t.Fatalf("Parse(%+v): %v", req, err)
	}
	return r
}

func TestParseRejects(t *testing.T) {
	now := time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)
	for name, req := range map[string]recur.Request{
		"nothing":           {},
		"two kinds":         {In: "20m", Every: "1h"},
		"days without time": {Every: "1h", Days: []string{"mon"}},
		"short delay":       {In: "30s"},
		"short interval":    {Every: "10s"},
		"bad time":          {Time: "9am"},
		"bad day":           {Time: "09:00", Days: []string{"someday"}},
		"bad zone":          {Time: "09:00", Timezone: "Mars/Olympus"},
		"past":              {At: "2026-10-07T09:00"},
		"garbled at":        {At: "tomorrow at nine"},
	} {
		if _, err := recur.Parse(req, "UTC", now); err == nil {
			t.Errorf("%s: accepted %+v", name, req)
		}
	}
}

func TestOnce(t *testing.T) {
	now := time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)
	in := mustParse(t, recur.Request{In: "20m"}, now)
	if next, ok := in.Next(now); !ok || !next.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("in 20m = %v %v", next, ok)
	}
	// A local time is read in the seat's zone (+11:00 in October).
	local := mustParse(t, recur.Request{At: "2026-10-09T15:00"}, now)
	if want := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC); !local.At.Equal(want) {
		t.Fatalf("local at = %v, want %v", local.At, want)
	}
	offset := mustParse(t, recur.Request{At: "2026-10-09T15:00:00+01:00"}, now)
	if want := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC); !offset.At.Equal(want) {
		t.Fatalf("offset at = %v", offset.At)
	}
	for _, s := range []string{"2026-10-09T15:00:00", "2026-10-09 15:00"} {
		if r := mustParse(t, recur.Request{At: s}, now); !r.At.Equal(local.At) {
			t.Fatalf("%s = %v", s, r.At)
		}
	}
	if _, ok := local.Next(local.At); ok {
		t.Fatal("one-off ran twice")
	}
	if got := local.Summary(); got != "once, at Fri 2026-10-09 15:00 AEDT (+11:00)" {
		t.Fatalf("summary = %q", got)
	}
}

func TestInterval(t *testing.T) {
	now := time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)
	r := mustParse(t, recur.Request{Every: "2h"}, now)
	got := r.Occurrences(now, 3)
	for i, want := range []time.Duration{2 * time.Hour, 4 * time.Hour, 6 * time.Hour} {
		if !got[i].Equal(now.Add(want)) {
			t.Fatalf("occurrence %d = %v", i, got[i])
		}
	}
	// After downtime the next run stays on the original cadence.
	if next, _ := r.Next(now.Add(5*time.Hour + 10*time.Minute)); !next.Equal(now.Add(6 * time.Hour)) {
		t.Fatalf("next after downtime = %v", next)
	}
	if n := r.CountBetween(now, now.Add(5*time.Hour+10*time.Minute), 100); n != 2 {
		t.Fatalf("missed = %d", n)
	}
	if r.Summary() != "every 2h" {
		t.Fatalf("summary = %q", r.Summary())
	}
}

func TestWeekdays(t *testing.T) {
	// Thursday 2026-10-08 13:00 in Melbourne.
	now := time.Date(2026, 10, 8, 13, 0, 0, 0, melbourne)
	r := mustParse(t, recur.Request{Time: "09:00", Days: []string{"weekdays"}}, now)
	var got []string
	for _, o := range r.Occurrences(now, 3) {
		got = append(got, o.In(melbourne).Format("Mon 15:04"))
	}
	if strings.Join(got, ", ") != "Fri 09:00, Mon 09:00, Tue 09:00" {
		t.Fatalf("occurrences = %v", got)
	}
	if r.Summary() != "every weekday at 09:00 (Australia/Melbourne)" {
		t.Fatalf("summary = %q", r.Summary())
	}
	daily := mustParse(t, recur.Request{Time: "18:30", Days: []string{"Monday", "tue", "wed", "thu", "fri", "sat", "sun"}}, now)
	if daily.Days != nil || daily.Summary() != "every day at 18:30 (Australia/Melbourne)" {
		t.Fatalf("all days = %+v %q", daily.Days, daily.Summary())
	}
	if next, _ := daily.Next(now); next.In(melbourne).Format("2006-01-02 15:04") != "2026-10-08 18:30" {
		t.Fatalf("today's run = %v", next)
	}
	mw := mustParse(t, recur.Request{Time: "07:15", Days: []string{"wed", "mon"}}, now)
	if mw.Summary() != "every Mon, Wed at 07:15 (Australia/Melbourne)" {
		t.Fatalf("summary = %q", mw.Summary())
	}
}

func TestCalendarAcrossDaylightSaving(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	// 2026-03-08: New York clocks go from 02:00 to 03:00.
	before := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	nine := mustParse(t, recur.Request{Time: "09:00", Timezone: "America/New_York"}, before)
	var got []string
	for _, o := range nine.Occurrences(before, 3) {
		got = append(got, recur.FormatLocal(o, ny))
	}
	want := "Sun 2026-03-08 09:00 EDT (-04:00), Mon 2026-03-09 09:00 EDT (-04:00), Tue 2026-03-10 09:00 EDT (-04:00)"
	if strings.Join(got, ", ") != want {
		t.Fatalf("09:00 across the change = %v", got)
	}
	// 02:30 does not exist that day: it runs at 03:30.
	gap := mustParse(t, recur.Request{Time: "02:30", Timezone: "America/New_York"}, before)
	if next, _ := gap.Next(before); recur.FormatLocal(next, ny) != "Sun 2026-03-08 03:30 EDT (-04:00)" {
		t.Fatalf("gap run = %s", recur.FormatLocal(next, ny))
	}
	// 2026-11-01: 01:30 happens twice; it runs once, at the first.
	fall := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	twice := mustParse(t, recur.Request{Time: "01:30", Timezone: "America/New_York"}, fall)
	occ := twice.Occurrences(fall, 2)
	if recur.FormatLocal(occ[0], ny) != "Sun 2026-11-01 01:30 EDT (-04:00)" || recur.FormatLocal(occ[1], ny) != "Mon 2026-11-02 01:30 EST (-05:00)" {
		t.Fatalf("repeated hour = %s, %s", recur.FormatLocal(occ[0], ny), recur.FormatLocal(occ[1], ny))
	}
}
