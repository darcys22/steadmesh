package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// SeatRuntimes reports the runtime view of the organisation's active seats,
// retiring ones included, optionally restricted to the given keys.
func (s *Store) SeatRuntimes(ctx context.Context, orgID string, keys []string) (map[string]runtimeapi.SeatRuntime, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id, s.key, l.generation, l.holder, l.expires_at, l.state, l.state_detail,
			l.adopted_revision, l.last_activity,
			(SELECT count(*) FROM deliveries d WHERE d.seat_id = s.id AND d.state IN ('pending', 'leased')),
			COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(d.created_at)) FROM deliveries d
				WHERE d.seat_id = s.id AND d.state IN ('pending', 'leased')), 0)::float8,
			(SELECT count(*) FROM deliveries d WHERE d.seat_id = s.id AND d.state = 'dead'),
			COALESCE((SELECT checkpoint_ref FROM sessions ss WHERE ss.seat_id = s.id ORDER BY updated_at DESC LIMIT 1), ''),
			CASE WHEN s.retiring_at IS NOT NULL THEN s.retire_by END
		FROM seats s JOIN execution_leases l ON l.seat_id = s.id
		WHERE s.organization_id = $1 AND s.retired_at IS NULL AND (cardinality($2::text[]) = 0 OR s.key = ANY($2))`,
		orgID, nonNil(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]runtimeapi.SeatRuntime{}
	for rows.Next() {
		var r runtimeapi.SeatRuntime
		if err := rows.Scan(&r.SeatID, &r.SeatKey, &r.LeaseGeneration, &r.LeaseHolder, &r.LeaseExpiresAt, &r.State,
			&r.StateDetail, &r.AdoptedRevision, &r.LastActivity, &r.PendingDeliveries, &r.OldestPendingAge,
			&r.DeadDeliveries, &r.LatestCheckpoint, &r.RetireBy); err != nil {
			return nil, err
		}
		out[r.SeatKey] = r
	}
	return out, rows.Err()
}

// SaveCheckpoint stores the seat's harness checkpoint (ADR-0005).
func (s *Store) SaveCheckpoint(ctx context.Context, f Fence, cp runtimeapi.Checkpoint) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (seat_id, harness_adapter, format_version, checkpoint_ref, guarantee, lease_generation)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (seat_id, harness_adapter) DO UPDATE SET format_version = EXCLUDED.format_version,
				checkpoint_ref = EXCLUDED.checkpoint_ref, guarantee = EXCLUDED.guarantee,
				lease_generation = EXCLUDED.lease_generation, updated_at = now()`,
			f.SeatID, cp.HarnessAdapter, cp.FormatVersion, cp.CheckpointRef, cp.Guarantee, f.Generation)
		return err
	})
}

// Checkpoints returns the seat's checkpoints, most recent first.
func (s *Store) Checkpoints(ctx context.Context, seatID string) ([]runtimeapi.Checkpoint, error) {
	rows, err := s.pool.Query(ctx, `SELECT harness_adapter, format_version, checkpoint_ref, guarantee FROM sessions
		WHERE seat_id = $1 ORDER BY updated_at DESC`, seatID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[runtimeapi.Checkpoint])
}

// SaveHandoff replaces the seat's portable handoff (§8.4).
func (s *Store) SaveHandoff(ctx context.Context, f Fence, h runtimeapi.Handoff) error {
	for _, ids := range [][]string{h.RecordIDs, h.PendingMessageIDs, h.OperationIDs} {
		for _, id := range ids {
			if _, err := uuid.Parse(id); err != nil {
				return fmt.Errorf("%w: %q is not an id", ErrInvalid, id)
			}
		}
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO handoffs (seat_id, objective, unresolved, record_ids, pending_message_ids, operation_ids, notes, lease_generation)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (seat_id) DO UPDATE SET objective = EXCLUDED.objective, unresolved = EXCLUDED.unresolved,
				record_ids = EXCLUDED.record_ids, pending_message_ids = EXCLUDED.pending_message_ids,
				operation_ids = EXCLUDED.operation_ids, notes = EXCLUDED.notes, lease_generation = EXCLUDED.lease_generation,
				updated_at = now()`,
			f.SeatID, h.Objective, nonNil(h.Unresolved), nonNil(h.RecordIDs), nonNil(h.PendingMessageIDs),
			nonNil(h.OperationIDs), h.Notes, f.Generation)
		return err
	})
}

// Handoff returns the seat's portable handoff, or nil.
func (s *Store) Handoff(ctx context.Context, seatID string) (*runtimeapi.Handoff, error) {
	h := &runtimeapi.Handoff{}
	err := s.pool.QueryRow(ctx, `SELECT objective, unresolved, record_ids::text[], pending_message_ids::text[], operation_ids::text[], notes
		FROM handoffs WHERE seat_id = $1`, seatID).Scan(&h.Objective, &h.Unresolved, &h.RecordIDs, &h.PendingMessageIDs, &h.OperationIDs, &h.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

// CreateProbe queues a synthetic probe message for the seat; the probe id is
// the message id. A retiring seat is not probed.
func (s *Store) CreateProbe(ctx context.Context, orgID, seatID string) (string, error) {
	var id string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats WHERE id = $1 AND organization_id = $2
			AND retired_at IS NULL AND retiring_at IS NULL)`,
			seatID, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		var err error
		id, _, err = systemMessage(ctx, tx, orgID, seatID, OriginProbe, "", "steadmesh synthetic readiness probe")
		return err
	})
	return id, err
}

// ProbeStatus reports a probe's progress from its delivery and execution.
// It passes when the runner acknowledged it as completed and no reported
// check failed.
func (s *Store) ProbeStatus(ctx context.Context, orgID, seatID, probeID string) (*runtimeapi.ProbeResponse, error) {
	if _, err := uuid.Parse(probeID); err != nil {
		return nil, ErrNotFound
	}
	out := &runtimeapi.ProbeResponse{ProbeID: probeID, Status: "pending"}
	var delivery, lastErr string
	err := s.pool.QueryRow(ctx, `SELECT d.state, d.last_error FROM messages m JOIN deliveries d ON d.message_id = m.id
		WHERE m.id = $1 AND m.organization_id = $2 AND m.origin = 'probe' AND d.seat_id = $3`, probeID, orgID, seatID).Scan(&delivery, &lastErr)
	if err != nil {
		return nil, notFound(err)
	}
	switch delivery {
	case "dead":
		out.Status, out.Error = "failed", lastErr
		return out, nil
	case "done":
	default:
		return out, nil
	}
	var execID, execState, execErr string
	if err := s.pool.QueryRow(ctx, `SELECT id, state, error FROM executions WHERE trigger_message_id = $1 ORDER BY started_at DESC LIMIT 1`,
		probeID).Scan(&execID, &execState, &execErr); err != nil {
		return nil, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT data->'checks' FROM execution_events WHERE execution_id = $1 AND data ? 'checks'
		ORDER BY id DESC LIMIT 1`, execID).Scan(&raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	failed := execState != "completed"
	if len(raw) > 0 {
		var checks map[string]any
		if json.Unmarshal(raw, &checks) == nil {
			out.Checks = map[string]string{}
			for k, v := range checks {
				str := fmt.Sprint(v)
				out.Checks[k] = str
				if l := strings.ToLower(str); strings.HasPrefix(l, "fail") || l == "false" {
					failed = true
				}
			}
		}
	}
	out.Status = "passed"
	if failed {
		out.Status, out.Error = "failed", execErr
	}
	return out, nil
}

// Gauges is a snapshot for the §14 operational gauges.
type Gauges struct {
	// SeatsByState counts active seats per organisation key and state.
	SeatsByState map[[2]string]int
	// OldestPending is the oldest undelivered message age per organisation key.
	OldestPending map[string]float64
}

// Gauges computes the gauge snapshot across active organisations.
func (s *Store) Gauges(ctx context.Context) (*Gauges, error) {
	g := &Gauges{SeatsByState: map[[2]string]int{}, OldestPending: map[string]float64{}}
	rows, err := s.pool.Query(ctx, `SELECT o.key, l.state, count(*) FROM seats s JOIN organizations o ON o.id = s.organization_id
		JOIN execution_leases l ON l.seat_id = s.id WHERE s.retired_at IS NULL AND o.deleted_at IS NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var org, state string
		var n int
		if err := rows.Scan(&org, &state, &n); err != nil {
			return nil, err
		}
		g.SeatsByState[[2]string{org, state}] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT o.key, COALESCE(EXTRACT(EPOCH FROM now() - min(d.created_at)), 0)::float8
		FROM organizations o JOIN seats s ON s.organization_id = o.id
		LEFT JOIN deliveries d ON d.seat_id = s.id AND d.state IN ('pending', 'leased')
		WHERE o.deleted_at IS NULL GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var org string
		var age float64
		if err := rows.Scan(&org, &age); err != nil {
			return nil, err
		}
		g.OldestPending[org] = age
	}
	return g, rows.Err()
}

// RecordConnectionCheck stores a verification result and the connection's
// credential refresh state for readiness (§5.4). Nothing secret is stored.
func (s *Store) RecordConnectionCheck(ctx context.Context, orgID, connection string, res runtimeapi.CheckResult, cred runtimeapi.CredentialStatus) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO connection_checks (organization_id, connection, ok, detail,
			credential_state, secret_version, credential_error, refreshed_at, previous_until, refresh_failing_since)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (organization_id, connection) DO UPDATE SET ok = EXCLUDED.ok, detail = EXCLUDED.detail,
			credential_state = EXCLUDED.credential_state, secret_version = EXCLUDED.secret_version,
			credential_error = EXCLUDED.credential_error, refreshed_at = EXCLUDED.refreshed_at,
			previous_until = EXCLUDED.previous_until, refresh_failing_since = EXCLUDED.refresh_failing_since,
			checked_at = now()`,
		orgID, connection, res.OK, res.Detail, cred.State, cred.SecretVersion, cred.Error, cred.RefreshedAt, cred.PreviousUntil, cred.FailingSince)
	return err
}
