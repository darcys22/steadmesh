package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Inbox limits (contracts.md, ADR-0006).
const (
	DeliveryLease        = 15 * time.Minute
	MaxAttempts          = 5
	MaxPendingDeliveries = 1000
	MaxEventBytes        = 8 << 10
)

// backoffSQL is the delay before attempt n+1 after n failed attempts:
// 1s, 8s, 64s, then capped at 5m.
const backoffSQL = `LEAST(300, power(8, GREATEST(attempts, 1) - 1)) * interval '1 second'`

// failAttempt is the SET clause that returns a leased delivery to the queue
// with backoff, or dead-letters it once its attempts are exhausted so it never
// blocks the seat. errParam is the placeholder number of the error text.
func failAttempt(errParam int) string {
	return fmt.Sprintf(`state = CASE WHEN attempts >= %d THEN 'dead' ELSE 'pending' END,
		next_attempt_at = now() + %s, leased_until = NULL, last_error = $%d, updated_at = now()`, MaxAttempts, backoffSQL, errParam)
}

// requeueStale returns deliveries leased by generations older than gen.
func requeueStale(ctx context.Context, tx pgx.Tx, seatID string, gen int64, reason string) error {
	rows, err := tx.Query(ctx, `UPDATE deliveries SET `+failAttempt(3)+`
		WHERE seat_id = $1 AND state = 'leased' AND lease_generation < $2 RETURNING execution_id`, seatID, gen, reason)
	if err != nil {
		return err
	}
	execs, err := pgx.CollectRows(rows, pgx.RowTo[*string])
	if err != nil {
		return err
	}
	return interruptExecutions(ctx, tx, execs, reason)
}

func interruptExecutions(ctx context.Context, tx pgx.Tx, ids []*string, reason string) error {
	for _, id := range ids {
		if id == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE executions SET state = 'interrupted', error = $2, finished_at = now()
			WHERE id = $1 AND state = 'running'`, *id, reason); err != nil {
			return err
		}
	}
	return nil
}

// RequeueExpired returns deliveries whose 15-minute lease expired to the queue
// with backoff and dead-letters exhausted ones. It returns the number of
// deliveries dead-lettered.
func (s *Store) RequeueExpired(ctx context.Context) (int, error) {
	dead := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE deliveries SET `+failAttempt(1)+`
			WHERE state = 'leased' AND leased_until < now() RETURNING execution_id, state`, "delivery lease expired")
		if err != nil {
			return err
		}
		var execs []*string
		for rows.Next() {
			var id *string
			var state string
			if err := rows.Scan(&id, &state); err != nil {
				return err
			}
			execs = append(execs, id)
			if state == "dead" {
				dead++
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return interruptExecutions(ctx, tx, execs, "delivery lease expired")
	})
	return dead, err
}

// LeaseNext atomically leases the seat's oldest eligible delivery. Within a
// conversation deliveries are handed out in order, and a conversation with a
// leased delivery is skipped. The execution row records the configuration
// revision and lease generation the turn runs under (A14). It returns nil when
// nothing is eligible.
func (s *Store) LeaseNext(ctx context.Context, f Fence) (*runtimeapi.InboxDelivery, error) {
	var out *runtimeapi.InboxDelivery
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var id int64
		var msgID string
		err := tx.QueryRow(ctx, `SELECT d.id, d.message_id FROM deliveries d JOIN messages m ON m.id = d.message_id
			WHERE d.seat_id = $1 AND d.state = 'pending' AND d.next_attempt_at <= now()
			  AND NOT EXISTS (SELECT 1 FROM deliveries d2 JOIN messages m2 ON m2.id = d2.message_id
				WHERE d2.seat_id = d.seat_id AND m2.conversation_id = m.conversation_id AND d2.id <> d.id
				  AND (d2.state = 'leased' OR (d2.state = 'pending' AND d2.id < d.id)))
			ORDER BY d.id LIMIT 1 FOR UPDATE OF d SKIP LOCKED`, f.SeatID).Scan(&id, &msgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		execID := uuid.NewString()
		var attempts int
		if err := tx.QueryRow(ctx, `UPDATE deliveries SET state = 'leased', attempts = attempts + 1, lease_generation = $2,
			leased_until = now() + make_interval(secs => $3), execution_id = $4, updated_at = now()
			WHERE id = $1 RETURNING attempts`, id, f.Generation, DeliveryLease.Seconds(), execID).Scan(&attempts); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO executions (id, seat_id, lease_generation, config_revision, harness_adapter, trigger_message_id)
			SELECT $1, s.id, $3, s.config_revision, COALESCE(s.manifest->'harness'->>'adapter', ''), $4 FROM seats s WHERE s.id = $2`,
			execID, f.SeatID, f.Generation, msgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE execution_leases SET last_activity = now() WHERE seat_id = $1`, f.SeatID); err != nil {
			return err
		}
		env, err := envelope(ctx, tx, msgID)
		if err != nil {
			return err
		}
		out = &runtimeapi.InboxDelivery{DeliveryID: id, ExecutionID: execID, Attempt: attempts, Message: *env}
		return nil
	})
	return out, err
}

func envelope(ctx context.Context, q pgx.Tx, msgID string) (*runtimeapi.Envelope, error) {
	e := &runtimeapi.Envelope{}
	var binding string
	err := q.QueryRow(ctx, `SELECT m.id, m.organization_id, m.conversation_id, m.origin, c.binding, m.origin_external_user,
			COALESCE(ss.key, ''), COALESCE(m.sender_seat_id::text, ''), COALESCE(rs.key, ''),
			COALESCE(m.parent_id::text, ''), COALESCE(m.correlation_id::text, ''), m.body, m.created_at
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN seats ss ON ss.id = m.sender_seat_id LEFT JOIN seats rs ON rs.id = m.recipient_seat_id
		WHERE m.id = $1`, msgID).Scan(&e.MessageID, &e.OrganizationID, &e.ConversationID, &e.Origin, &binding,
		&e.ExternalUserID, &e.SenderSeat, &e.SenderSeatID, &e.RecipientSeat, &e.ParentID, &e.CorrelationID, &e.Body, &e.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	switch e.Origin {
	case OriginHuman:
		e.Binding = binding
		e.ReplyRoute = "binding:" + binding
	case OriginSeat:
		e.ReplyRoute = "seat:" + e.SenderSeat
	}
	return e, nil
}

// Ack completes a leased delivery and its execution. A failed outcome counts
// as an attempt and is retried with backoff until dead-lettered.
func (s *Store) Ack(ctx context.Context, f Fence, deliveryID int64, req runtimeapi.InboxAckRequest) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		set := `state = 'done', leased_until = NULL, last_error = $4, updated_at = now()`
		execState := "completed"
		if req.Outcome == "failed" {
			set, execState = failAttempt(4), "failed"
		}
		tag, err := tx.Exec(ctx, `UPDATE deliveries SET `+set+`
			WHERE id = $1 AND seat_id = $2 AND state = 'leased' AND lease_generation = $3 AND execution_id = $5`,
			deliveryID, f.SeatID, f.Generation, req.Error, req.ExecutionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE executions SET state = $2, error = $3, finished_at = now() WHERE id = $1`,
			req.ExecutionID, execState, req.Error); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE execution_leases SET last_activity = now() WHERE seat_id = $1`, f.SeatID)
		return err
	})
}

// AppendEvents records execution events for one of the seat's executions.
// Oversized payloads are replaced by a marker (ADR-0006).
func (s *Store) AppendEvents(ctx context.Context, f Fence, executionID string, events []runtimeapi.ExecutionEvent) error {
	if _, err := uuid.Parse(executionID); err != nil {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM executions WHERE id = $1 AND seat_id = $2)`, executionID, f.SeatID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		for _, ev := range events {
			data := []byte(ev.Data)
			if len(data) == 0 || !json.Valid(data) {
				data = []byte(`{}`)
			} else if len(data) > MaxEventBytes {
				data = fmt.Appendf(nil, `{"truncated":true,"bytes":%d}`, len(data))
			}
			at := ev.Time
			if at.IsZero() {
				at = time.Now()
			}
			if _, err := tx.Exec(ctx, `INSERT INTO execution_events (execution_id, kind, correlation_id, data, created_at)
				VALUES ($1, $2, $3, $4, $5)`, executionID, ev.Kind, ev.CorrelationID, data, at); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE execution_leases SET last_activity = now() WHERE seat_id = $1`, f.SeatID)
		return err
	})
}

// PendingDeliveries counts the seat's undelivered (pending or leased) messages.
func (s *Store) PendingDeliveries(ctx context.Context, seatID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE seat_id = $1 AND state IN ('pending', 'leased')`, seatID).Scan(&n)
	return n, err
}

// insertDelivery queues msgID for seatID. With enforceCap, a full inbox is
// rejected with ErrInboxFull; otherwise the delivery is always accepted and
// full reports whether the cap was exceeded.
func insertDelivery(ctx context.Context, tx pgx.Tx, msgID, seatID string, enforceCap bool) (full bool, err error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE seat_id = $1 AND state IN ('pending', 'leased')`, seatID).Scan(&n); err != nil {
		return false, err
	}
	full = n >= MaxPendingDeliveries
	if full && enforceCap {
		return true, ErrInboxFull
	}
	_, err = tx.Exec(ctx, `INSERT INTO deliveries (message_id, seat_id) VALUES ($1, $2)`, msgID, seatID)
	return full, err
}
