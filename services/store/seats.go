package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// LeaseTTL is the execution lease lifetime without renewal (contracts.md).
const LeaseTTL = 30 * time.Second

// Seat is an active seat with its current policy revision.
type Seat struct {
	ID             string
	OrganizationID string
	Key            string
	ConfigRevision string
	PolicyRevision int64
	Manifest       compile.SeatManifest
	// RetireBy is set while the seat is retiring: it takes no new messages
	// and winds down until then.
	RetireBy *time.Time
}

// SeatByServiceAccount maps an authenticated ServiceAccount to its active seat.
func (s *Store) SeatByServiceAccount(ctx context.Context, namespace, serviceAccount string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT s.id FROM seats s JOIN organizations o ON o.id = s.organization_id
		WHERE o.namespace = $1 AND s.service_account = $2 AND s.retired_at IS NULL AND o.deleted_at IS NULL`,
		namespace, serviceAccount).Scan(&id)
	return id, notFound(err)
}

// Seat loads an active seat and its committed manifest. Retired seats and seats
// of deleted organisations are not found, which revokes their access.
func (s *Store) Seat(ctx context.Context, seatID string) (*Seat, error) {
	return scanSeat(s.pool.QueryRow(ctx, seatQuery+` AND s.id = $1`, seatID))
}

// SeatByKey loads an active seat by organisation and key.
func (s *Store) SeatByKey(ctx context.Context, orgID, key string) (*Seat, error) {
	return scanSeat(s.pool.QueryRow(ctx, seatQuery+` AND s.organization_id = $1 AND s.key = $2`, orgID, key))
}

const seatQuery = `SELECT s.id, s.organization_id, s.key, s.config_revision, s.policy_revision, s.manifest,
	CASE WHEN s.retiring_at IS NOT NULL THEN s.retire_by END
	FROM seats s JOIN organizations o ON o.id = s.organization_id
	WHERE s.retired_at IS NULL AND o.deleted_at IS NULL`

func scanSeat(row pgx.Row) (*Seat, error) {
	st := &Seat{}
	var raw []byte
	if err := row.Scan(&st.ID, &st.OrganizationID, &st.Key, &st.ConfigRevision, &st.PolicyRevision, &raw, &st.RetireBy); err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(raw, &st.Manifest); err != nil {
		return nil, err
	}
	return st, nil
}

// AcquireLease grants the seat's execution lease to podUID when it is expired,
// unheld or already held by podUID, incrementing the generation. Deliveries
// leased by an older generation go back to the queue as a failed attempt.
func (s *Store) AcquireLease(ctx context.Context, seatID, podUID string) (*runtimeapi.LeaseResponse, error) {
	out := &runtimeapi.LeaseResponse{SeatID: seatID, TTLSeconds: int(LeaseTTL.Seconds())}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE execution_leases SET generation = generation + 1, holder = $2,
				expires_at = now() + make_interval(secs => $3), state = 'Provisioning', state_detail = '', updated_at = now()
			WHERE seat_id = $1 AND (holder = '' OR holder = $2 OR expires_at < now())
			RETURNING generation, expires_at`, seatID, podUID, LeaseTTL.Seconds()).Scan(&out.Generation, &out.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return requeueStale(ctx, tx, seatID, out.Generation, "lease reacquired")
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RenewLease extends the lease held by podUID at generation gen.
func (s *Store) RenewLease(ctx context.Context, seatID, podUID string, gen int64) (*runtimeapi.LeaseResponse, error) {
	out := &runtimeapi.LeaseResponse{SeatID: seatID, Generation: gen, TTLSeconds: int(LeaseTTL.Seconds())}
	err := s.pool.QueryRow(ctx, `UPDATE execution_leases SET expires_at = now() + make_interval(secs => $4), updated_at = now()
		WHERE seat_id = $1 AND generation = $2 AND holder = $3 RETURNING expires_at`,
		seatID, gen, podUID, LeaseTTL.Seconds()).Scan(&out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFenced
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReleaseLease gives up the lease at generation gen and records Stopped.
func (s *Store) ReleaseLease(ctx context.Context, seatID string, gen int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE execution_leases SET holder = '', expires_at = 'epoch', state = 'Stopped',
		last_activity = now(), updated_at = now() WHERE seat_id = $1 AND generation = $2`, seatID, gen)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFenced
	}
	return nil
}

// ReportState records the runner's execution state and adopted revision.
func (s *Store) ReportState(ctx context.Context, f Fence, req runtimeapi.StateRequest) error {
	tag, err := s.pool.Exec(ctx, `UPDATE execution_leases SET state = $3, state_detail = $4,
		adopted_revision = CASE WHEN $5 = '' THEN adopted_revision ELSE $5 END, last_activity = now(), updated_at = now()
		WHERE seat_id = $1 AND generation = $2`, f.SeatID, f.Generation, req.State, req.Detail, req.AdoptedRevision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFenced
	}
	return nil
}

// FenceSeat expires the seat's lease and increments its generation so the
// previous holder's calls are rejected. The controller calls it only after
// confirming the previous Pod is gone (contracts.md).
func (s *Store) FenceSeat(ctx context.Context, orgID, seatID string, expected int64) (*runtimeapi.LeaseResponse, error) {
	out := &runtimeapi.LeaseResponse{SeatID: seatID, TTLSeconds: int(LeaseTTL.Seconds())}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE execution_leases l SET generation = generation + 1, holder = '', expires_at = 'epoch',
				state = 'Recovering', state_detail = 'fenced by controller', updated_at = now()
			FROM seats s WHERE s.id = l.seat_id AND l.seat_id = $1 AND s.organization_id = $2 AND l.generation = $3
			RETURNING l.generation, l.expires_at`, seatID, orgID, expected).Scan(&out.Generation, &out.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats WHERE id = $1 AND organization_id = $2)`, seatID, orgID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return requeueStale(ctx, tx, seatID, out.Generation, "execution fenced")
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SeatOrganization returns the organisation of any seat, active or retired.
func (s *Store) SeatOrganization(ctx context.Context, seatID string) (string, error) {
	var org string
	err := s.pool.QueryRow(ctx, `SELECT organization_id FROM seats WHERE id = $1`, seatID).Scan(&org)
	return org, notFound(err)
}
