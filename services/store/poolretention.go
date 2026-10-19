package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// PurgeExpiredPoolState applies an explicit positive private-data retention
// policy only to fully retired participants. Shared work, evidence, messages,
// audit identities and pool admission/budget history are deliberately retained.
func (s *Store) PurgeExpiredPoolState(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT p.organization_id,p.seat_id FROM native_pool_members p JOIN seats s ON s.id=p.seat_id WHERE p.retention_seconds>0 AND p.retained_until<=now() AND p.purged_at IS NULL AND s.retired_at IS NOT NULL ORDER BY p.retained_until LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type candidate struct{ org, seat string }
	candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := r.Scan(&c.org, &c.seat)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err = s.inTx(ctx, func(tx pgx.Tx) error {
			var org string
			if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, c.org).Scan(&org); err == pgx.ErrNoRows {
				return nil
			} else if err != nil {
				return err
			}
			var seat string
			err := tx.QueryRow(ctx, `SELECT s.id FROM seats s JOIN native_pool_members p ON p.seat_id=s.id WHERE s.id=$1 AND s.retired_at IS NOT NULL AND p.retention_seconds>0 AND p.retained_until<=now() AND p.purged_at IS NULL FOR UPDATE OF s,p`, c.seat).Scan(&seat)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			for _, q := range []string{
				`UPDATE artifacts SET store_id=NULL WHERE store_id IN (SELECT id FROM memory_stores WHERE owner_seat_id=$1)`,
				`DELETE FROM work_publications WHERE record_id IN (SELECT r.id FROM memory_records r JOIN memory_stores ms ON ms.id=r.store_id WHERE ms.owner_seat_id=$1)`,
				`DELETE FROM memory_revisions WHERE store_id IN (SELECT id FROM memory_stores WHERE owner_seat_id=$1)`,
				`DELETE FROM memory_records WHERE store_id IN (SELECT id FROM memory_stores WHERE owner_seat_id=$1)`,
				`DELETE FROM memory_stores WHERE owner_seat_id=$1`,
				`DELETE FROM context_instruction_proposals WHERE owner_seat_id=$1`,
				`DELETE FROM participant_context_history WHERE seat_id=$1`,
				`DELETE FROM participant_context WHERE seat_id=$1`,
				`DELETE FROM sessions WHERE seat_id=$1`,
				`DELETE FROM handoffs WHERE seat_id=$1`,
				`UPDATE native_pool_members SET purged_at=now() WHERE seat_id=$1`,
			} {
				if _, err := tx.Exec(ctx, q, seat); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
