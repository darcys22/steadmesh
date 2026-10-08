package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/recur"
)

// MaxAutomationsPerSeat bounds a seat's active and paused automations.
const MaxAutomationsPerSeat = 50

// Automation statuses.
const (
	AutomationActive    = "active"
	AutomationPaused    = "paused"
	AutomationCompleted = "completed"
	AutomationDeleted   = "deleted"
)

// maxMissedCount bounds the count of occurrences skipped during downtime.
const maxMissedCount = 1000

// Automation is a named instruction a seat runs for itself on a schedule
// (§9.3). Each run queues a schedule-origin message to the seat.
type Automation struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Instruction string     `json:"instruction"`
	Rule        recur.Rule `json:"rule"`
	Status      string     `json:"status"`
	NextRunAt   *time.Time `json:"next_run_at,omitempty"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	Runs        int        `json:"runs"`
	SkippedRuns int        `json:"skipped_runs"`
	MaxRuns     *int       `json:"max_runs,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

const automationColumns = `id, name, instruction, rule, status, next_run_at, last_run_at, runs, skipped_runs, max_runs, run_until, created_at, updated_at`

func scanAutomation(r pgx.Row) (Automation, error) {
	var a Automation
	var rule []byte
	err := r.Scan(&a.ID, &a.Name, &a.Instruction, &rule, &a.Status, &a.NextRunAt, &a.LastRunAt, &a.Runs, &a.SkippedRuns,
		&a.MaxRuns, &a.Until, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(rule, &a.Rule)
}

// sameName finds the seat's live automation with this name.
func sameName(ctx context.Context, tx pgx.Tx, seatID, name, except string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM automations WHERE seat_id = $1 AND lower(name) = lower($2) AND status IN ('active', 'paused')
		AND id::text <> $3`, seatID, name, except).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CreateAutomation saves an automation for the seat itself. A live
// automation with the same name is a conflict, so agents update rather than
// duplicate.
func (s *Store) CreateAutomation(ctx context.Context, f Fence, orgID string, a Automation) (*Automation, error) {
	a.ID = uuid.NewString()
	rule, err := json.Marshal(a.Rule)
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM automations WHERE seat_id = $1 AND status IN ('active', 'paused')`, f.SeatID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxAutomationsPerSeat {
			return fmt.Errorf("%w: at most %d active or paused automations per seat; delete one first", ErrInvalid, MaxAutomationsPerSeat)
		}
		if id, err := sameName(ctx, tx, f.SeatID, a.Name, ""); err != nil {
			return err
		} else if id != "" {
			return fmt.Errorf("%w: an automation named %q already exists (id %s); change it with automations.update instead", ErrConflict, a.Name, id)
		}
		row := tx.QueryRow(ctx, `INSERT INTO automations (id, organization_id, seat_id, name, instruction, rule, status, next_run_at, max_runs, run_until)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+automationColumns,
			a.ID, orgID, f.SeatID, a.Name, a.Instruction, rule, a.Status, a.NextRunAt, a.MaxRuns, a.Until)
		out, err := scanAutomation(row)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: an automation named %q already exists", ErrConflict, a.Name)
		}
		a = out
		return err
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Automations lists the seat's active and paused automations, and with
// completed also those that will not run again.
func (s *Store) Automations(ctx context.Context, seatID string, completed bool) ([]Automation, error) {
	statuses := []string{AutomationActive, AutomationPaused}
	if completed {
		statuses = append(statuses, AutomationCompleted)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+automationColumns+` FROM automations
		WHERE seat_id = $1 AND status = ANY($2) ORDER BY next_run_at NULLS LAST, lower(name), id`, seatID, statuses)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Automation, error) { return scanAutomation(r) })
}

// UpdateAutomation applies change to one of the seat's automations that has
// not been deleted, and saves it.
func (s *Store) UpdateAutomation(ctx context.Context, f Fence, id string, change func(*Automation) error) (*Automation, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out Automation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		a, err := scanAutomation(tx.QueryRow(ctx, `SELECT `+automationColumns+` FROM automations
			WHERE id = $1 AND seat_id = $2 AND status <> 'deleted' FOR UPDATE`, id, f.SeatID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := change(&a); err != nil {
			return err
		}
		if a.Status == AutomationActive || a.Status == AutomationPaused {
			if other, err := sameName(ctx, tx, f.SeatID, a.Name, a.ID); err != nil {
				return err
			} else if other != "" {
				return fmt.Errorf("%w: another automation is named %q (id %s)", ErrConflict, a.Name, other)
			}
		}
		rule, err := json.Marshal(a.Rule)
		if err != nil {
			return err
		}
		out, err = scanAutomation(tx.QueryRow(ctx, `UPDATE automations SET name = $2, instruction = $3, rule = $4, status = $5,
			next_run_at = $6, max_runs = $7, run_until = $8, updated_at = now() WHERE id = $1 RETURNING `+automationColumns,
			a.ID, a.Name, a.Instruction, rule, a.Status, a.NextRunAt, a.MaxRuns, a.Until))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAutomation stops one of the seat's automations; it runs no more.
// A run already queued is not withdrawn.
func (s *Store) DeleteAutomation(ctx context.Context, f Fence, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE automations SET status = 'deleted', next_run_at = NULL, updated_at = now()
			WHERE id = $1 AND seat_id = $2 AND status <> 'deleted'`, id, f.SeatID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// FireDue runs up to limit due automations of active seats and returns the
// number of runs queued. Each run queues a schedule-origin message,
// deduplicated on (automation, scheduled time) so a repeated pass never
// queues it twice. While the previous run's message is still waiting or in
// progress the occurrence is skipped rather than piled up. Occurrences
// missed while the platform was down are not replayed: one catch-up run
// reports how many were missed, and the schedule resumes at the next future
// occurrence.
func (s *Store) FireDue(ctx context.Context, now time.Time, limit int) (int, error) {
	queued := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.organization_id, a.seat_id, a.last_message_id, `+prefixed("a.", automationColumns)+`
			FROM automations a JOIN seats s ON s.id = a.seat_id
			WHERE a.status = 'active' AND a.next_run_at <= $1 AND s.retired_at IS NULL AND s.retiring_at IS NULL
			ORDER BY a.next_run_at LIMIT $2 FOR UPDATE OF a SKIP LOCKED`, now, limit)
		if err != nil {
			return err
		}
		type due struct {
			org, seat string
			lastMsg   *string
			a         Automation
		}
		dues, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
			var d due
			var rule []byte
			a := &d.a
			err := r.Scan(&d.org, &d.seat, &d.lastMsg, &a.ID, &a.Name, &a.Instruction, &rule, &a.Status, &a.NextRunAt, &a.LastRunAt,
				&a.Runs, &a.SkippedRuns, &a.MaxRuns, &a.Until, &a.CreatedAt, &a.UpdatedAt)
			if err == nil {
				err = json.Unmarshal(rule, &a.Rule)
			}
			return d, err
		})
		if err != nil {
			return err
		}
		for _, d := range dues {
			a := d.a
			scheduled := *a.NextRunAt
			missed := a.Rule.CountBetween(scheduled, now, maxMissedCount)
			var busy bool
			if d.lastMsg != nil {
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE message_id = $1 AND state IN ('pending', 'leased'))`,
					*d.lastMsg).Scan(&busy); err != nil {
					return err
				}
			}
			runs, skipped, msgID := a.Runs, a.SkippedRuns+missed, d.lastMsg
			if busy {
				skipped++
			} else {
				last := a.MaxRuns != nil && runs+1 >= *a.MaxRuns
				next, more := a.Rule.Next(now)
				last = last || !more || a.Rule.Kind == recur.KindOnce || (a.Until != nil && next.After(*a.Until))
				key := fmt.Sprintf("automation:%s:%d", a.ID, scheduled.UnixMicro())
				id, inserted, err := systemMessage(ctx, tx, d.org, d.seat, OriginSchedule, key, runBody(a, runs+1, scheduled, missed, last))
				if err != nil {
					return err
				}
				// A replayed trigger finds its message already queued.
				if inserted {
					runs++
					queued++
					msgID = &id
				}
			}
			status, next := AutomationActive, (*time.Time)(nil)
			if n, ok := a.Rule.Next(now); ok && a.Rule.Kind != recur.KindOnce && (a.MaxRuns == nil || runs < *a.MaxRuns) && (a.Until == nil || !n.After(*a.Until)) {
				next = &n
			} else {
				status = AutomationCompleted
			}
			if _, err := tx.Exec(ctx, `UPDATE automations SET status = $2, next_run_at = $3, last_run_at = CASE WHEN $4 THEN last_run_at ELSE $5 END,
				runs = $6, skipped_runs = $7, last_message_id = $8, updated_at = now() WHERE id = $1`,
				a.ID, status, next, busy, now, runs, skipped, msgID); err != nil {
				return err
			}
		}
		return nil
	})
	return queued, err
}

// runBody is the message a run delivers to its seat.
func runBody(a Automation, run int, scheduled time.Time, missed int, last bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled run of your automation %q (id %s): run %d", a.Name, a.ID, run)
	if a.MaxRuns != nil {
		fmt.Fprintf(&b, " of %d", *a.MaxRuns)
	}
	fmt.Fprintf(&b, ", scheduled for %s (%s).", recur.FormatLocal(scheduled, a.Rule.Location()), a.Rule.Summary())
	if missed > 0 {
		fmt.Fprintf(&b, " %d later occurrence(s) were missed while the platform was unavailable and will not be run.", missed)
	}
	if last && a.Rule.Kind != recur.KindOnce {
		b.WriteString(" This is the last run; the automation then completes.")
	}
	b.WriteString("\n\nInstruction:\n")
	b.WriteString(a.Instruction)
	b.WriteString("\n\nThis run comes from your own schedule, not from a new request. Carry out the instruction. " +
		"Message people only when the instruction or what you find calls for it.")
	return b.String()
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}
