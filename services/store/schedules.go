package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxSchedulesPerSeat bounds a seat's active wake schedules.
const MaxSchedulesPerSeat = 50

// Schedule is a durable wake trigger (§9.3).
type Schedule struct {
	ID            string     `json:"id"`
	Schedule      string     `json:"schedule"`
	Note          string     `json:"note,omitempty"`
	NextTriggerAt time.Time  `json:"next_trigger_at"`
	LastFiredAt   *time.Time `json:"last_fired_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// CreateSchedule registers a wake schedule for the seat itself.
func (s *Store) CreateSchedule(ctx context.Context, f Fence, orgID, schedule string, next time.Time, note string) (*Schedule, error) {
	out := &Schedule{ID: uuid.NewString(), Schedule: schedule, Note: note, NextTriggerAt: next}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM wake_schedules WHERE seat_id = $1 AND active`, f.SeatID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSchedulesPerSeat {
			return fmt.Errorf("%w: at most %d active schedules per seat", ErrInvalid, MaxSchedulesPerSeat)
		}
		return tx.QueryRow(ctx, `INSERT INTO wake_schedules (id, organization_id, seat_id, schedule, next_trigger_at, note, author_seat_id)
			VALUES ($1, $2, $3, $4, $5, $6, $3) RETURNING created_at`, out.ID, orgID, f.SeatID, schedule, next, note).Scan(&out.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Schedules lists the seat's active schedules.
func (s *Store) Schedules(ctx context.Context, seatID string) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, schedule, note, next_trigger_at, last_fired_at, created_at FROM wake_schedules
		WHERE seat_id = $1 AND active ORDER BY next_trigger_at, id`, seatID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Schedule, error) {
		var sc Schedule
		err := r.Scan(&sc.ID, &sc.Schedule, &sc.Note, &sc.NextTriggerAt, &sc.LastFiredAt, &sc.CreatedAt)
		return sc, err
	})
}

// CancelSchedule deactivates one of the seat's schedules.
func (s *Store) CancelSchedule(ctx context.Context, f Fence, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE wake_schedules SET active = false WHERE id = $1 AND seat_id = $2 AND active`, id, f.SeatID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// NextFunc computes a schedule's next trigger after firing at fired; ok=false
// means the schedule is finished.
type NextFunc func(schedule string, fired, now time.Time) (next time.Time, ok bool)

// FireDue fires up to limit due schedules of active seats. Each fire inserts a
// schedule-origin message and delivery, deduplicated on (schedule, fire time)
// so a repeated fire never queues a second wake. It returns the number of
// wakes queued.
func (s *Store) FireDue(ctx context.Context, now time.Time, limit int, next NextFunc) (int, error) {
	fired := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT w.id, w.organization_id, w.seat_id, w.schedule, w.next_trigger_at, w.note
			FROM wake_schedules w JOIN seats s ON s.id = w.seat_id
			WHERE w.active AND w.next_trigger_at <= $1 AND s.retired_at IS NULL
			ORDER BY w.next_trigger_at LIMIT $2 FOR UPDATE OF w SKIP LOCKED`, now, limit)
		if err != nil {
			return err
		}
		type due struct {
			id, org, seat, schedule, note string
			at                            time.Time
		}
		dues, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
			var d due
			err := r.Scan(&d.id, &d.org, &d.seat, &d.schedule, &d.at, &d.note)
			return d, err
		})
		if err != nil {
			return err
		}
		for _, d := range dues {
			body := "Scheduled wake"
			if d.note != "" {
				body += ": " + d.note
			}
			key := fmt.Sprintf("wake:%s:%d", d.id, d.at.UnixMicro())
			_, inserted, err := systemMessage(ctx, tx, d.org, d.seat, OriginSchedule, key, body)
			if err != nil {
				return err
			}
			if inserted {
				fired++
			}
			nt, ok := next(d.schedule, d.at, now)
			if !ok {
				nt = d.at
			}
			if _, err := tx.Exec(ctx, `UPDATE wake_schedules SET last_fired_at = $2, next_trigger_at = $3, active = $4 WHERE id = $1`,
				d.id, now, nt, ok); err != nil {
				return err
			}
		}
		return nil
	})
	return fired, err
}
