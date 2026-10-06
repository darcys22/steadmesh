package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Operation statuses (§10.3).
const (
	OpPending   = "pending"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
	OpUnknown   = "unknown"
)

// NewOperation is a connector side effect about to be attempted.
type NewOperation struct {
	OrganizationID string
	SeatID         string
	ExecutionID    string
	Connection     string
	Operation      string
	Target         string
	RequestHash    string
	IdempotencyKey string
}

const operationColumns = `id, connection, operation, target, idempotency_key, status, external_receipt,
	COALESCE(result, 'null'::jsonb), error, created_at`

func scanOperation(row pgx.Row) (*runtimeapi.Operation, error) {
	op := &runtimeapi.Operation{}
	var result []byte
	err := row.Scan(&op.ID, &op.Connection, &op.Operation, &op.Target, &op.IdempotencyKey, &op.Status,
		&op.ExternalReceipt, &result, &op.Error, &op.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if string(result) != "null" {
		op.Result = result
	}
	return op, nil
}

// BeginOperation records a pending operation before its side effect, in a
// fenced transaction. If the idempotency key was already used, the existing
// operation is returned with created=false and must not be re-executed.
func (s *Store) BeginOperation(ctx context.Context, f Fence, in NewOperation) (op *runtimeapi.Operation, created bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		id := uuid.NewString()
		tag, err := tx.Exec(ctx, `INSERT INTO connector_operations (id, organization_id, seat_id, execution_id, lease_generation,
				connection, operation, target, request_hash, idempotency_key)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (organization_id, connection, idempotency_key) DO NOTHING`,
			id, in.OrganizationID, in.SeatID, in.ExecutionID, f.Generation, in.Connection, in.Operation, in.Target,
			in.RequestHash, in.IdempotencyKey)
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		op, err = scanOperation(tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM connector_operations
			WHERE organization_id = $1 AND connection = $2 AND idempotency_key = $3`,
			in.OrganizationID, in.Connection, in.IdempotencyKey))
		return err
	})
	return op, created, err
}

// FinishOperation records an operation's outcome. Its result is recorded
// regardless of later fencing: it describes what happened externally.
func (s *Store) FinishOperation(ctx context.Context, id, status, receipt string, result json.RawMessage, errText string, attempts int) error {
	if len(result) == 0 {
		result = nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE connector_operations SET status = $2, external_receipt = $3, result = $4,
		error = $5, attempts = attempts + $6, updated_at = now() WHERE id = $1`, id, status, receipt, result, errText, attempts)
	return err
}

// Operation loads one of the seat's operations.
func (s *Store) Operation(ctx context.Context, orgID, seatID, id string) (*runtimeapi.Operation, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	return scanOperation(s.pool.QueryRow(ctx, `SELECT `+operationColumns+` FROM connector_operations
		WHERE organization_id = $1 AND seat_id = $2 AND id = $3`, orgID, seatID, id))
}

// UnknownOperations lists the seat's operations whose outcome is unknown, for
// agent-led reconciliation (§10.3).
func (s *Store) UnknownOperations(ctx context.Context, seatID string, limit int) ([]runtimeapi.Operation, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+operationColumns+` FROM connector_operations
		WHERE seat_id = $1 AND status = 'unknown' ORDER BY created_at DESC LIMIT $2`, seatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runtimeapi.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *op)
	}
	return out, rows.Err()
}

// ExpireStaleOperations marks operations left pending by a crashed attempt as
// unknown: the side effect may have happened, so they are never replayed.
func (s *Store) ExpireStaleOperations(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE connector_operations o SET status = 'unknown',
			error = 'attempt interrupted; outcome unknown', updated_at = now()
		WHERE status = 'pending' AND updated_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM outbox WHERE operation_id = o.id AND state IN ('pending', 'sending'))`, olderThan.Seconds())
	return tag.RowsAffected(), err
}

// OutboxItem is a claimed outbound message.
type OutboxItem struct {
	ID             int64
	OrganizationID string
	MessageID      string
	OperationID    string
	Connection     string
	Binding        string
	ExternalRef    string
	Body           string
	Attempts       int
}

// Outbox states. A claimed row is "sending" until resolved; a claim that is
// never resolved (crash mid-send) becomes unknown, never a blind resend.
const (
	OutboxPending = "pending"
	OutboxSending = "sending"
	OutboxSent    = "sent"
	OutboxUnknown = "unknown"
	OutboxDead    = "dead"
)

const outboxClaim = 5 * time.Minute

// ClaimOutbox claims up to limit due outbound messages and records a pending
// connector operation for each before anything is sent.
func (s *Store) ClaimOutbox(ctx context.Context, limit int) ([]OutboxItem, error) {
	var out []OutboxItem
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o.id, o.organization_id, o.message_id, COALESCE(o.operation_id::text, ''), o.connection,
				m.recipient_binding, c.external_ref, m.body, o.attempts, COALESCE(m.sender_seat_id::text, '')
			FROM outbox o JOIN messages m ON m.id = o.message_id JOIN conversations c ON c.id = m.conversation_id
			WHERE o.state = 'pending' AND o.next_attempt_at <= now()
			ORDER BY o.id LIMIT $1 FOR UPDATE OF o SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var senders []string
		for rows.Next() {
			var it OutboxItem
			var sender string
			if err := rows.Scan(&it.ID, &it.OrganizationID, &it.MessageID, &it.OperationID, &it.Connection,
				&it.Binding, &it.ExternalRef, &it.Body, &it.Attempts, &sender); err != nil {
				return err
			}
			out = append(out, it)
			senders = append(senders, sender)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			it := &out[i]
			if it.OperationID == "" {
				it.OperationID = uuid.NewString()
				if _, err := tx.Exec(ctx, `INSERT INTO connector_operations (id, organization_id, seat_id, connection, operation,
						target, request_hash, idempotency_key)
					VALUES ($1, $2, NULLIF($3, '')::uuid, $4, 'channel.reply', $5, $6, $7)`,
					it.OperationID, it.OrganizationID, senders[i], it.Connection, it.Binding, "message:"+it.MessageID,
					"outbox:"+it.MessageID); err != nil {
					return err
				}
			}
			it.Attempts++
			if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'sending', attempts = $2, operation_id = $3,
				next_attempt_at = now() + make_interval(secs => $4), updated_at = now() WHERE id = $1`,
				it.ID, it.Attempts, it.OperationID, outboxClaim.Seconds()); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ResolveOutbox records the outcome of a send. state is one of the outbox
// states; for OutboxPending the row is retried after retryIn.
func (s *Store) ResolveOutbox(ctx context.Context, it OutboxItem, state, receipt, errText string, retryIn time.Duration) error {
	opStatus := map[string]string{OutboxSent: OpSucceeded, OutboxDead: OpFailed, OutboxUnknown: OpUnknown, OutboxPending: OpPending}[state]
	if opStatus == "" {
		return errors.New("invalid outbox state " + state)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE outbox SET state = $2, last_error = $3,
			next_attempt_at = now() + make_interval(secs => $4), updated_at = now() WHERE id = $1`,
			it.ID, state, errText, retryIn.Seconds()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE connector_operations SET status = $2, external_receipt = $3, error = $4,
			attempts = attempts + 1, updated_at = now() WHERE id = $1`, it.OperationID, opStatus, receipt, errText)
		return err
	})
}

// ExpireSending marks outbound messages whose claim lapsed without a recorded
// outcome as unknown.
func (s *Store) ExpireSending(ctx context.Context) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE outbox SET state = 'unknown', last_error = 'send interrupted; outcome unknown', updated_at = now()
			WHERE state = 'sending' AND next_attempt_at < now() RETURNING operation_id`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[*string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id == nil {
				continue
			}
			n++
			if _, err := tx.Exec(ctx, `UPDATE connector_operations SET status = 'unknown', error = 'send interrupted; outcome unknown',
				updated_at = now() WHERE id = $1`, *id); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}
