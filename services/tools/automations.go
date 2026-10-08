package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/darcys22/steadmesh/pkg/recur"
	"github.com/darcys22/steadmesh/services/store"
)

const scheduleProps = `"in":{"type":"string","description":"Run once after this delay, e.g. \"20m\" or \"2h\"."},` +
	`"at":{"type":"string","description":"Run once at this moment: local time \"2026-10-09T15:00\" in the time zone, or RFC 3339 with an offset."},` +
	`"every":{"type":"string","description":"Run repeatedly at this interval of elapsed time, e.g. \"30m\" or \"2h\" (at least 1m). For a time of day use time instead."},` +
	`"time":{"type":"string","description":"Run at this local time of day, 24-hour \"HH:MM\", on days."},` +
	`"days":{"type":"array","items":{"type":"string"},"description":"With time: mon, tue, wed, thu, fri, sat, sun, \"weekdays\", \"weekends\" or \"daily\". Omit for every day (or, when updating, to keep the days)."},` +
	`"timezone":{"type":"string","description":"IANA time zone for at and time, e.g. Australia/Melbourne. Defaults to yours."},` +
	`"until":{"type":["string","null"],"description":"No runs after this moment (local time or RFC 3339). null removes it."},` +
	`"max_runs":{"type":["integer","null"],"minimum":1,"description":"Complete after this many runs have been queued. null removes it."}`

func (r *Registry) automationTools() []*tool {
	return []*tool{
		{name: "automations.create", description: "Save a named instruction that you will carry out yourself on a schedule: once after a delay (in), once at a moment (at), " +
			"repeatedly at an interval (every), or at a local time of day on chosen days (time, days). Use it for any reminder, follow-up, check-back or " +
			"recurring task instead of waiting, then finish your turn. Each run queues a turn for you carrying the instruction; if you are busy then, it runs " +
			"when you are free. Times without an offset are in your time zone unless timezone is given. Check the returned schedule and next_runs before " +
			"telling anyone it is set: success means it is scheduled, not that the future work succeeded. If an automation for this purpose already exists, " +
			"change it with automations.update instead of creating another.",
			schema: `{"type":"object","properties":{"name":{"type":"string","minLength":1,"maxLength":100,"description":"Short unique name for its purpose, e.g. \"Team progress check\". Leave the schedule out; it is shown separately and can change."},` +
				`"instruction":{"type":"string","minLength":1,"maxLength":4096,"description":"What to do on each run, written to make sense on its own later: what to check, who to tell, and when to stay quiet."},` +
				scheduleProps + `},"required":["name","instruction"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.automationCreate},
		{name: "automations.list", description: "List your automations with their schedule, status, next runs and run counts. Check it before creating one, " +
			"and to find the id of one to change or delete.",
			schema:  `{"type":"object","properties":{"include_completed":{"type":"boolean","description":"Also list automations that will not run again."}},"additionalProperties":false}`,
			allowed: always, handle: r.automationList},
		{name: "automations.update", description: "Change one of your automations by id: its name, instruction, schedule or end condition, or pause it " +
			"(status paused) and resume it (status active). Giving in, at or every replaces the schedule; time, days or timezone adjust a " +
			"time-of-day schedule and keep what you leave out (moving a weekday check to 10:00 keeps it on weekdays). Resuming skips runs that fell due while paused. Returns the saved automation and its next runs.",
			schema: `{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string","minLength":1,"maxLength":100},` +
				`"instruction":{"type":"string","minLength":1,"maxLength":4096},"status":{"type":"string","enum":["active","paused"]},` +
				scheduleProps + `},"required":["id"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.automationUpdate},
		{name: "automations.delete", description: "Delete one of your automations so it never runs again. A run that was already queued still arrives.",
			schema:   `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.automationDelete},
		{name: "clock.now", description: "The current date and time in your time zone, or in timezone. Use it before working out a date or a relative time.",
			schema:  `{"type":"object","properties":{"timezone":{"type":"string","description":"IANA time zone; defaults to yours."}},"additionalProperties":false}`,
			allowed: always, handle: r.clockNow},
	}
}

// seatZone is the seat's time zone (its human's for a representative).
func seatZone(c *Call) string {
	if tz := c.Seat.Manifest.Timezone; tz != "" {
		return tz
	}
	return "UTC"
}

type automationArgs struct {
	ID          string          `json:"id"`
	Name        *string         `json:"name"`
	Instruction *string         `json:"instruction"`
	Status      string          `json:"status"`
	In          string          `json:"in"`
	At          string          `json:"at"`
	Every       string          `json:"every"`
	Time        string          `json:"time"`
	Days        []string        `json:"days"`
	Timezone    string          `json:"timezone"`
	Until       json.RawMessage `json:"until"`
	MaxRuns     json.RawMessage `json:"max_runs"`
}

func (a *automationArgs) schedule() recur.Request {
	return recur.Request{In: a.In, At: a.At, Every: a.Every, Time: a.Time, Days: a.Days, Timezone: a.Timezone}
}

func (a *automationArgs) touchesSchedule() bool {
	return a.In != "" || a.At != "" || a.Every != "" || a.Time != "" || a.Days != nil || a.Timezone != ""
}

// applyLimits sets until and max_runs when given; JSON null clears them.
func (a *automationArgs) applyLimits(out *store.Automation, loc *time.Location, now time.Time) error {
	if len(a.Until) > 0 {
		if bytes.Equal(a.Until, []byte("null")) {
			out.Until = nil
		} else {
			var s string
			if err := json.Unmarshal(a.Until, &s); err != nil {
				return toolErr("invalid", "until must be a time or null")
			}
			t, err := recur.ParseMoment(s, loc)
			if err != nil {
				return toolErr("invalid", "until must be RFC 3339 or local time \"2026-10-09T15:00\"")
			}
			if !t.After(now) {
				return toolErr("invalid", "until %s is not in the future", recur.FormatLocal(t, loc))
			}
			out.Until = &t
		}
	}
	if len(a.MaxRuns) > 0 {
		if bytes.Equal(a.MaxRuns, []byte("null")) {
			out.MaxRuns = nil
		} else {
			var n int
			if err := json.Unmarshal(a.MaxRuns, &n); err != nil || n < 1 {
				return toolErr("invalid", "max_runs must be a positive integer or null")
			}
			out.MaxRuns = &n
		}
	}
	return nil
}

// schedule sets the next run of an active automation from now, so runs that
// fell due while it was paused are skipped.
func reschedule(a *store.Automation, now time.Time) error {
	if a.Status != store.AutomationActive {
		a.NextRunAt = nil
		return nil
	}
	if a.MaxRuns != nil && a.Runs >= *a.MaxRuns {
		return toolErr("invalid", "it has already run %d time(s); raise or remove max_runs to run it again", a.Runs)
	}
	next, ok := a.Rule.Next(now)
	if !ok {
		return toolErr("invalid", "its time has passed; give a new schedule with in or at")
	}
	if a.Until != nil && next.After(*a.Until) {
		return toolErr("invalid", "its next run (%s) would be after until", recur.FormatLocal(next, a.Rule.Location()))
	}
	a.NextRunAt = &next
	return nil
}

func (r *Registry) automationCreate(ctx context.Context, c *Call) (any, error) {
	var a automationArgs
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.Name == nil || a.Instruction == nil {
		return nil, toolErr("invalid", "name and instruction are required")
	}
	now := time.Now()
	rule, err := recur.Parse(a.schedule(), seatZone(c), now)
	if err != nil {
		return nil, toolErr("invalid", "%s", err.Error())
	}
	auto := store.Automation{Name: *a.Name, Instruction: *a.Instruction, Rule: rule, Status: store.AutomationActive}
	if err := a.applyLimits(&auto, rule.Location(), now); err != nil {
		return nil, err
	}
	if err := reschedule(&auto, now); err != nil {
		return nil, err
	}
	out, err := r.d.Store.CreateAutomation(ctx, c.fence(), c.Seat.OrganizationID, auto)
	if err != nil {
		return nil, err
	}
	return automationView(*out, "Saved. Each run queues a turn for you with the instruction; future runs have not happened yet."), nil
}

func (r *Registry) automationList(ctx context.Context, c *Call) (any, error) {
	var a struct {
		IncludeCompleted bool `json:"include_completed"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	list, err := r.d.Store.Automations(ctx, c.Seat.ID, a.IncludeCompleted)
	if err != nil {
		return nil, err
	}
	views := make([]map[string]any, 0, len(list))
	for _, x := range list {
		views = append(views, automationView(x, ""))
	}
	return map[string]any{"automations": views, "timezone": seatZone(c)}, nil
}

func (r *Registry) automationUpdate(ctx context.Context, c *Call) (any, error) {
	var a automationArgs
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	now := time.Now()
	out, err := r.d.Store.UpdateAutomation(ctx, c.fence(), a.ID, func(x *store.Automation) error {
		if a.Name != nil {
			x.Name = *a.Name
		}
		if a.Instruction != nil {
			x.Instruction = *a.Instruction
		}
		if a.touchesSchedule() {
			req := a.schedule()
			// Adjusting a time-of-day schedule keeps what was not given.
			if req.In == "" && req.At == "" && req.Every == "" {
				if x.Rule.Kind == recur.KindCalendar {
					if req.Time == "" {
						req.Time = x.Rule.Time
					}
					if req.Days == nil {
						req.Days = x.Rule.Days
					}
				} else if req.Time == "" {
					return toolErr("invalid", "days and timezone alone only adjust a time-of-day schedule; give in, at, every or time")
				}
			}
			if req.Timezone == "" {
				req.Timezone = x.Rule.Timezone
			}
			rule, err := recur.Parse(req, seatZone(c), now)
			if err != nil {
				return toolErr("invalid", "%s", err.Error())
			}
			x.Rule = rule
			if x.Status == store.AutomationCompleted {
				x.Status = store.AutomationActive
			}
		}
		switch a.Status {
		case "":
		case store.AutomationActive, store.AutomationPaused:
			x.Status = a.Status
		default:
			return toolErr("invalid", "status must be active or paused")
		}
		if err := a.applyLimits(x, x.Rule.Location(), now); err != nil {
			return err
		}
		if x.Status == store.AutomationCompleted {
			return toolErr("invalid", "it has completed; give it a new schedule or status active to run it again")
		}
		return reschedule(x, now)
	})
	if err != nil {
		return nil, err
	}
	return automationView(*out, "Saved."), nil
}

func (r *Registry) automationDelete(ctx context.Context, c *Call) (any, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := r.d.Store.DeleteAutomation(ctx, c.fence(), a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": a.ID}, nil
}

func (r *Registry) clockNow(_ context.Context, c *Call) (any, error) {
	var a struct {
		Timezone string `json:"timezone"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	tz := a.Timezone
	if tz == "" {
		tz = seatZone(c)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, toolErr("invalid", "unknown time zone %q; use an IANA name such as Australia/Melbourne", tz)
	}
	now := time.Now().In(loc)
	return map[string]any{"now": recur.FormatLocal(now, loc), "rfc3339": now.Format(time.RFC3339), "timezone": loc.String()}, nil
}

// automationView is what agents see: times in the automation's time zone,
// a readable schedule and the next few runs.
func automationView(a store.Automation, note string) map[string]any {
	loc := a.Rule.Location()
	v := map[string]any{
		"id": a.ID, "name": a.Name, "instruction": a.Instruction, "status": a.Status,
		"schedule": a.Rule.Summary(), "timezone": a.Rule.Timezone, "runs": a.Runs,
	}
	if a.SkippedRuns > 0 {
		v["skipped_runs"] = a.SkippedRuns
	}
	if a.MaxRuns != nil {
		v["max_runs"] = *a.MaxRuns
	}
	if a.Until != nil {
		v["until"] = recur.FormatLocal(*a.Until, loc)
	}
	if a.LastRunAt != nil {
		v["last_run"] = recur.FormatLocal(*a.LastRunAt, loc)
	}
	if a.NextRunAt != nil {
		next := []string{}
		remaining := -1
		if a.MaxRuns != nil {
			remaining = *a.MaxRuns - a.Runs
		}
		for _, t := range append([]time.Time{*a.NextRunAt}, a.Rule.Occurrences(*a.NextRunAt, 2)...) {
			if remaining == 0 || (a.Until != nil && t.After(*a.Until)) {
				break
			}
			next = append(next, recur.FormatLocal(t, loc))
			remaining--
		}
		v["next_runs"] = next
	}
	if note != "" {
		v["note"] = note
	}
	return v
}
