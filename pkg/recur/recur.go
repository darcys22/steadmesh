// Package recur describes when an automation runs: once, at a fixed interval,
// or at a local time of day on chosen weekdays in an IANA time zone. It
// validates requests, computes occurrences (including across daylight-saving
// transitions) and describes schedules for people.
package recur

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	// Time zones are needed in minimal images without /usr/share/zoneinfo.
	_ "time/tzdata"
)

// MinInterval is the shortest delay or interval.
const MinInterval = time.Minute

// Rule kinds.
const (
	KindOnce     = "once"
	KindInterval = "interval"
	KindCalendar = "calendar"
)

// maxCalendarDays bounds how far ahead a calendar rule is searched.
const maxCalendarDays = 8

var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Request is how an agent asks for a schedule. Exactly one of In, At, Every
// or Time is set; Days and Timezone qualify Time (and Timezone a local At).
type Request struct {
	// In is a delay from now, e.g. "20m".
	In string `json:"in,omitempty"`
	// At is a moment: RFC 3339 with an offset, or local wall-clock time
	// "2006-01-02T15:04" in Timezone.
	At string `json:"at,omitempty"`
	// Every is a fixed interval of elapsed time, at least MinInterval.
	Every string `json:"every,omitempty"`
	// Time is a local wall-clock time "15:04" repeated on Days in Timezone.
	Time string `json:"time,omitempty"`
	// Days are three-letter weekdays (mon..sun), "weekdays" or "weekends";
	// empty means every day.
	Days []string `json:"days,omitempty"`
	// Timezone is an IANA name; empty means the seat's time zone.
	Timezone string `json:"timezone,omitempty"`
}

// Rule is a validated, stored schedule.
type Rule struct {
	Kind string `json:"kind"`
	// At is the one-off moment (once).
	At time.Time `json:"at,omitzero"`
	// Every and Anchor define an interval: runs at Anchor + n*Every.
	Every  string    `json:"every,omitempty"`
	Anchor time.Time `json:"anchor,omitzero"`
	// Time and Days define a calendar rule in Timezone.
	Time     string   `json:"time,omitempty"`
	Days     []string `json:"days,omitempty"`
	Timezone string   `json:"timezone"`
}

// Parse validates a request. defaultTZ applies when the request names no
// time zone; now anchors "in" and "every".
func Parse(req Request, defaultTZ string, now time.Time) (Rule, error) {
	set := 0
	for _, v := range []string{req.In, req.At, req.Every, req.Time} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return Rule{}, errors.New("give exactly one of in, at, every or time")
	}
	if len(req.Days) > 0 && req.Time == "" {
		return Rule{}, errors.New("days applies only with time")
	}
	tzName := req.Timezone
	if tzName == "" {
		tzName = defaultTZ
	}
	if tzName == "" {
		tzName = "UTC"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return Rule{}, fmt.Errorf("unknown time zone %q; use an IANA name such as Australia/Melbourne", tzName)
	}
	r := Rule{Timezone: loc.String()}
	switch {
	case req.In != "":
		d, err := time.ParseDuration(req.In)
		if err != nil || d < MinInterval {
			return Rule{}, fmt.Errorf("in must be a duration of at least %s, e.g. \"20m\" or \"2h\"", MinInterval)
		}
		r.Kind, r.At = KindOnce, now.Add(d).Truncate(time.Second).UTC()
	case req.At != "":
		t, err := ParseMoment(req.At, loc)
		if err != nil {
			return Rule{}, errors.New("at must be RFC 3339 (2026-10-09T15:00:00+11:00) or local time (2026-10-09T15:00) in the time zone")
		}
		if !t.After(now) {
			return Rule{}, fmt.Errorf("at %s is not in the future", t.In(loc).Format(time.RFC3339))
		}
		r.Kind, r.At = KindOnce, t.UTC()
	case req.Every != "":
		d, err := time.ParseDuration(req.Every)
		if err != nil || d < MinInterval {
			return Rule{}, fmt.Errorf("every must be a duration of at least %s, e.g. \"30m\" or \"2h\"; use time and days for a time of day", MinInterval)
		}
		r.Kind, r.Every, r.Anchor = KindInterval, d.String(), now.Truncate(time.Second).UTC()
	default:
		if _, err := time.Parse("15:04", req.Time); err != nil {
			return Rule{}, errors.New("time must be a 24-hour local time \"HH:MM\", e.g. \"09:00\"")
		}
		days, err := normaliseDays(req.Days)
		if err != nil {
			return Rule{}, err
		}
		r.Kind, r.Time, r.Days = KindCalendar, req.Time, days
	}
	return r, nil
}

// localLayouts are the wall-clock forms accepted for a moment.
var localLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"}

// ParseMoment reads RFC 3339 with an offset, or a local wall-clock time
// ("2026-10-09T15:00", optionally with seconds or a space) in loc.
func ParseMoment(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time", s)
}

func normaliseDays(in []string) ([]string, error) {
	set := map[string]bool{}
	for _, d := range in {
		switch d = strings.ToLower(strings.TrimSpace(d)); {
		case d == "weekdays":
			for _, w := range weekdays[1:6] {
				set[w] = true
			}
		case d == "weekends":
			set["sat"], set["sun"] = true, true
		case d == "daily" || d == "every day" || d == "everyday":
			for _, w := range weekdays {
				set[w] = true
			}
		case len(d) >= 3 && slices.Contains(weekdays, d[:3]):
			set[d[:3]] = true
		default:
			return nil, fmt.Errorf("unknown day %q; use mon..sun, weekdays, weekends or daily", d)
		}
	}
	if len(set) == 7 {
		return nil, nil
	}
	var out []string
	for _, w := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if set[w] {
			out = append(out, w)
		}
	}
	return out, nil
}

// Location returns the rule's time zone.
func (r Rule) Location() *time.Location {
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Next returns the first occurrence strictly after t; ok is false when there
// is none.
func (r Rule) Next(t time.Time) (time.Time, bool) {
	switch r.Kind {
	case KindOnce:
		return r.At, r.At.After(t)
	case KindInterval:
		d, err := time.ParseDuration(r.Every)
		if err != nil || d <= 0 {
			return time.Time{}, false
		}
		if t.Before(r.Anchor) {
			return r.Anchor.Add(d), true
		}
		n := t.Sub(r.Anchor)/d + 1
		return r.Anchor.Add(n * d), true
	case KindCalendar:
		loc := r.Location()
		hm, _ := time.Parse("15:04", r.Time)
		day := t.In(loc)
		for i := range maxCalendarDays {
			d := day.AddDate(0, 0, i)
			if len(r.Days) > 0 && !slices.Contains(r.Days, weekdays[d.Weekday()]) {
				continue
			}
			if at := wallClock(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), loc); at.After(t) {
				return at, true
			}
		}
	}
	return time.Time{}, false
}

// wallClock returns the instant a local wall-clock time names. When the time
// occurs twice (clocks go back) it is the first; when it does not occur
// (clocks go forward) it is moved forward by the length of the gap, so
// 02:30 on a day that skips 02:00-03:00 becomes 03:30.
func wallClock(y int, m time.Month, d, hh, mm int, loc *time.Location) time.Time {
	w := time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
	offset := func(at time.Time) time.Duration {
		_, off := at.In(loc).Zone()
		return time.Duration(off) * time.Second
	}
	before, after := w.Add(-offset(w.Add(-24*time.Hour))), w.Add(-offset(w.Add(24*time.Hour)))
	var valid []time.Time
	for _, c := range []time.Time{before, after} {
		l := c.In(loc)
		if l.Year() == y && l.Month() == m && l.Day() == d && l.Hour() == hh && l.Minute() == mm {
			valid = append(valid, c)
		}
	}
	switch {
	case len(valid) == 2 && valid[1].Before(valid[0]):
		return valid[1]
	case len(valid) > 0:
		return valid[0]
	}
	// In a gap the offset before the transition is the smaller one; applying
	// it lands after the transition, shifted by the gap.
	return before
}

// Occurrences lists up to n occurrences strictly after t.
func (r Rule) Occurrences(t time.Time, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		next, ok := r.Next(t)
		if !ok {
			break
		}
		out = append(out, next)
		t = next
	}
	return out
}

// CountBetween counts occurrences in (from, to], up to limit.
func (r Rule) CountBetween(from, to time.Time, limit int) int {
	n := 0
	for n < limit {
		next, ok := r.Next(from)
		if !ok || next.After(to) {
			break
		}
		n++
		from = next
	}
	return n
}

// Summary describes the rule for people, e.g. "every weekday at 09:00
// (Australia/Melbourne)".
func (r Rule) Summary() string {
	switch r.Kind {
	case KindOnce:
		return "once, at " + FormatLocal(r.At, r.Location())
	case KindInterval:
		d, _ := time.ParseDuration(r.Every)
		return "every " + humanDuration(d)
	case KindCalendar:
		return fmt.Sprintf("%s at %s (%s)", describeDays(r.Days), r.Time, r.Timezone)
	}
	return r.Kind
}

func describeDays(days []string) string {
	switch {
	case len(days) == 0:
		return "every day"
	case slices.Equal(days, []string{"mon", "tue", "wed", "thu", "fri"}):
		return "every weekday"
	case slices.Equal(days, []string{"sat", "sun"}):
		return "every weekend day"
	}
	names := make([]string, len(days))
	for i, d := range days {
		names[i] = strings.ToUpper(d[:1]) + d[1:]
	}
	return "every " + strings.Join(names, ", ")
}

func humanDuration(d time.Duration) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	s = strings.TrimSuffix(s, "0m")
	if s == "" {
		return d.String()
	}
	return s
}

// FormatLocal renders an instant for people and agents with its weekday,
// zone abbreviation and UTC offset, e.g. "Mon 2026-10-13 09:00 AEDT (+11:00)".
func FormatLocal(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon 2006-01-02 15:04 MST (-07:00)")
}
