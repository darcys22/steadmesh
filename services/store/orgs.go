package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// SyncInput is a resolved organisation to establish identities for.
type SyncInput struct {
	Namespace string
	Key       string
	SourceUID string
	Manifest  *compile.Manifest
	// RetirementGrace is how long a seat removed from the manifest may wind
	// down before it is retired; zero retires it at the next retirement pass.
	RetirementGrace time.Duration
}

// SyncOrganization idempotently upserts the organisation, its seats and its
// memory stores in one transaction (§6.1). Seats missing from the manifest
// start retiring (retire.go): they take no new messages and get a bounded
// wind-down before they are retired with their data retained. A seat added
// back while retiring carries on. A seat's policy revision is bumped when its
// authority changes; because tools read the committed seat manifest on every
// call, the new policy is in force when this returns (A13).
func (s *Store) SyncOrganization(ctx context.Context, in SyncInput) (*runtimeapi.SyncResponse, error) {
	orgJSON, err := json.Marshal(in.Manifest)
	if err != nil {
		return nil, err
	}
	out := &runtimeapi.SyncResponse{Seats: map[string]runtimeapi.SeatIdentity{}}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var orgID string
		err := tx.QueryRow(ctx, `
			INSERT INTO organizations (id, key, namespace, source_uid, active_revision, manifest)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (namespace, key) DO UPDATE SET source_uid = EXCLUDED.source_uid,
				active_revision = EXCLUDED.active_revision, manifest = EXCLUDED.manifest,
				deleted_at = NULL, updated_at = now()
			RETURNING id`,
			uuid.NewString(), in.Key, in.Namespace, in.SourceUID, in.Manifest.Digest, orgJSON).Scan(&orgID)
		if err != nil {
			return fmt.Errorf("upsert organisation: %w", err)
		}
		out.OrganizationID = orgID

		active, err := activeSeats(ctx, tx, orgID)
		if err != nil {
			return err
		}
		retiring := map[string]string{} // personal store key -> retiring owner seat id
		for _, key := range sortedKeys(active) {
			seat := active[key]
			if _, ok := in.Manifest.Seats[key]; ok {
				continue
			}
			rs, err := beginRetirement(ctx, tx, orgID, key, seat, in.RetirementGrace)
			if err != nil {
				return err
			}
			if out.Retiring == nil {
				out.Retiring = map[string]runtimeapi.RetiringSeat{}
			}
			out.Retiring[key] = rs
			if seat.manifest.PersonalMemory != "" {
				retiring[seat.manifest.PersonalMemory] = seat.id
			}
		}

		adopted := map[string]string{} // personal store key -> adopting seat id
		for _, key := range sortedKeys(in.Manifest.Seats) {
			sm := in.Manifest.Seats[key]
			id, rev, err := upsertSeat(ctx, tx, orgID, in.Key, sm, active[key])
			if err != nil {
				return err
			}
			if active[key] == nil && sm.AdoptFrom != "" && sm.PersonalMemory != "" {
				adopted[sm.PersonalMemory] = id
			}
			out.Seats[key] = runtimeapi.SeatIdentity{SeatID: id, ServiceAccount: names.Seat(in.Key, key), PolicyRevision: rev}
		}
		return syncStores(ctx, tx, orgID, in.Manifest, out.Seats, adopted, retiring)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type activeSeat struct {
	id       string
	manifest compile.SeatManifest
	policy   int64
	// retireBy is set while the seat is retiring.
	retireBy *time.Time
}

func activeSeats(ctx context.Context, tx pgx.Tx, orgID string) (map[string]*activeSeat, error) {
	rows, err := tx.Query(ctx, `SELECT id, key, manifest, policy_revision, retire_by FROM seats
		WHERE organization_id = $1 AND retired_at IS NULL FOR UPDATE`, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]*activeSeat{}
	for rows.Next() {
		var key string
		var raw []byte
		a := &activeSeat{}
		if err := rows.Scan(&a.id, &key, &raw, &a.policy, &a.retireBy); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &a.manifest); err != nil {
			return nil, fmt.Errorf("seat %s manifest: %w", key, err)
		}
		out[key] = a
	}
	return out, rows.Err()
}

// authority is the part of a seat manifest that grants access. A change to it
// is a new policy revision.
func authority(sm compile.SeatManifest) string {
	b, _ := json.Marshal(struct {
		Capabilities    []compile.Capability
		SendTo          []compile.RouteEdge
		ReceiveFrom     []compile.RouteEdge
		ChannelBindings []string
		PersonalMemory  string
		Teams           []string
	}{sm.Capabilities, sm.SendTo, sm.ReceiveFrom, sm.ChannelBindings, sm.PersonalMemory, sm.Teams})
	return string(b)
}

func upsertSeat(ctx context.Context, tx pgx.Tx, orgID, orgKey string, sm compile.SeatManifest, cur *activeSeat) (string, int64, error) {
	raw, err := json.Marshal(sm)
	if err != nil {
		return "", 0, err
	}
	if cur != nil {
		if cur.retireBy != nil {
			if err := cancelRetirement(ctx, tx, orgID, cur.id); err != nil {
				return "", 0, err
			}
		}
		rev := cur.policy
		if authority(cur.manifest) != authority(sm) {
			rev++
		}
		_, err := tx.Exec(ctx, `UPDATE seats SET manifest = $2, config_revision = $3, policy_revision = $4,
			updated_at = now() WHERE id = $1`, cur.id, raw, sm.ConfigRevision, rev)
		return cur.id, rev, err
	}
	var adoptFrom *string
	if sm.AdoptFrom != "" {
		if err := checkAdoption(ctx, tx, orgID, sm); err != nil {
			return "", 0, err
		}
		adoptFrom = &sm.AdoptFrom
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO seats (id, organization_id, key, service_account, config_revision, manifest, adopted_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, orgID, sm.Key, names.Seat(orgKey, sm.Key), sm.ConfigRevision, raw, adoptFrom); err != nil {
		return "", 0, fmt.Errorf("create seat %s: %w", sm.Key, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_leases (seat_id) VALUES ($1)`, id); err != nil {
		return "", 0, err
	}
	if adoptFrom != nil {
		if err := transferSeatData(ctx, tx, orgID, *adoptFrom, id); err != nil {
			return "", 0, err
		}
	}
	return id, 1, nil
}

// checkAdoption allows adoption only from a retired seat of this organisation
// with the same key that nobody has adopted yet (§4.1).
func checkAdoption(ctx context.Context, tx pgx.Tx, orgID string, sm compile.SeatManifest) error {
	if _, err := uuid.Parse(sm.AdoptFrom); err != nil {
		return fmt.Errorf("%w: seats.%s.adopt_from: %q is not a seat id", ErrInvalid, sm.Key, sm.AdoptFrom)
	}
	var key string
	var retired, taken bool
	err := tx.QueryRow(ctx, `SELECT key, retired_at IS NOT NULL, EXISTS (SELECT 1 FROM seats a WHERE a.adopted_from = s.id)
		FROM seats s WHERE id = $1 AND organization_id = $2 FOR UPDATE`, sm.AdoptFrom, orgID).Scan(&key, &retired, &taken)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: seats.%s.adopt_from: no seat %s in this organisation", ErrInvalid, sm.Key, sm.AdoptFrom)
	case err != nil:
		return err
	case !retired:
		return fmt.Errorf("%w: seats.%s.adopt_from: seat %s is not retired", ErrInvalid, sm.Key, sm.AdoptFrom)
	case key != sm.Key:
		return fmt.Errorf("%w: seats.%s.adopt_from: seat %s has key %q", ErrInvalid, sm.Key, sm.AdoptFrom, key)
	case taken:
		return fmt.Errorf("%w: seats.%s.adopt_from: seat %s was already adopted", ErrInvalid, sm.Key, sm.AdoptFrom)
	}
	return nil
}

// transferSeatData attaches the retired seat's personal stores, private
// conversation history and portable handoff to the adopting identity.
func transferSeatData(ctx context.Context, tx pgx.Tx, orgID, oldID, newID string) error {
	if _, err := tx.Exec(ctx, `UPDATE memory_stores SET owner_seat_id = $1, policy_revision = policy_revision + 1
		WHERE owner_seat_id = $2 AND organization_id = $3`, newID, oldID, orgID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET participants = array_replace(participants, $2::uuid, $1::uuid)
		WHERE organization_id = $3 AND $2::uuid = ANY(participants)`, newID, oldID, orgID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO handoffs (seat_id, objective, unresolved, record_ids, pending_message_ids, operation_ids, notes, lease_generation)
		SELECT $1, objective, unresolved, record_ids, pending_message_ids, operation_ids, notes, 0 FROM handoffs WHERE seat_id = $2`, newID, oldID)
	return err
}

// retireSeat stops new delivery, interrupts active work and revokes the lease;
// all rows are retained (§5.5). A graceful retirement (retire.go) hands back
// what the seat held first.
func retireSeat(ctx context.Context, tx pgx.Tx, seatID, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE seats SET retired_at = now(), updated_at = now() WHERE id = $1`, seatID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE wake_schedules SET active = false WHERE seat_id = $1 AND active`, seatID); err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE deliveries SET state = 'dead', last_error = $2, updated_at = now() WHERE seat_id = $1 AND state IN ('pending', 'leased')`,
		`UPDATE executions SET state = 'interrupted', error = $2, finished_at = now() WHERE seat_id = $1 AND state = 'running'`,
		`UPDATE execution_leases SET generation = generation + 1, holder = '', expires_at = 'epoch',
			state = 'Retired', state_detail = $2, updated_at = now() WHERE seat_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, seatID, reason); err != nil {
			return fmt.Errorf("retire seat %s: %w", seatID, err)
		}
	}
	return nil
}

type storeRow struct {
	id           string
	owner        *string
	ownerRetired bool
}

// syncStores creates, updates and retires memory stores. A personal store
// still owned by a retired seat is never handed to a different identity: it is
// retired (data retained per its retention) and a fresh store takes the key
// (A18). An explicit adoption restores the adopted seat's store instead. A
// retiring seat keeps its personal store until it is retired, even when the
// manifest no longer declares it.
func syncStores(ctx context.Context, tx pgx.Tx, orgID string, m *compile.Manifest, seats map[string]runtimeapi.SeatIdentity,
	adopted, retiring map[string]string) error {
	owners := map[string]string{}
	for key, sm := range m.Seats {
		if sm.PersonalMemory != "" {
			owners[sm.PersonalMemory] = seats[key].SeatID
		}
	}
	// A retiring owner wins: its private memory never passes to another
	// identity (A18); it is retired with the seat.
	for key, id := range retiring {
		owners[key] = id
	}
	for _, key := range sortedKeys(m.Spec.MemoryStores) {
		ms := m.Spec.MemoryStores[key]
		var owner *string
		if id, ok := owners[key]; ok {
			owner = &id
		}
		cur, err := activeStore(ctx, tx, orgID, key)
		if err != nil {
			return err
		}
		if seatID, ok := adopted[key]; ok {
			if cur != nil && (cur.owner == nil || *cur.owner != seatID) {
				if err := retireStore(ctx, tx, cur.id); err != nil {
					return err
				}
				cur = nil
			}
			if cur == nil {
				tag, err := tx.Exec(ctx, `UPDATE memory_stores SET retired_at = NULL, retention = $4, policy_revision = policy_revision + 1
					WHERE id = (SELECT id FROM memory_stores WHERE organization_id = $1 AND key = $2 AND owner_seat_id = $3
						ORDER BY created_at DESC LIMIT 1)`, orgID, key, seatID, ms.Retention)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 1 {
					continue
				}
			}
		}
		if cur != nil && cur.owner != nil && cur.ownerRetired && (owner == nil || *owner != *cur.owner) {
			if err := retireStore(ctx, tx, cur.id); err != nil {
				return err
			}
			cur = nil
		}
		if cur == nil {
			if _, err := tx.Exec(ctx, `INSERT INTO memory_stores (id, organization_id, key, retention, owner_seat_id) VALUES ($1, $2, $3, $4, $5)`,
				uuid.NewString(), orgID, key, ms.Retention, owner); err != nil {
				return fmt.Errorf("create memory store %s: %w", key, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_stores SET retention = $2, owner_seat_id = $3,
			policy_revision = policy_revision + CASE WHEN owner_seat_id IS DISTINCT FROM $3 THEN 1 ELSE 0 END WHERE id = $1`,
			cur.id, ms.Retention, owner); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id, key FROM memory_stores WHERE organization_id = $1 AND retired_at IS NULL`, orgID)
	if err != nil {
		return err
	}
	var removed []string
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return err
		}
		_, declared := m.Spec.MemoryStores[key]
		if _, keep := retiring[key]; !declared && !keep {
			removed = append(removed, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range removed {
		if err := retireStore(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func activeStore(ctx context.Context, tx pgx.Tx, orgID, key string) (*storeRow, error) {
	r := &storeRow{}
	err := tx.QueryRow(ctx, `SELECT ms.id, ms.owner_seat_id, COALESCE(s.retired_at IS NOT NULL, false)
		FROM memory_stores ms LEFT JOIN seats s ON s.id = ms.owner_seat_id
		WHERE ms.organization_id = $1 AND ms.key = $2 AND ms.retired_at IS NULL FOR UPDATE OF ms`, orgID, key).
		Scan(&r.id, &r.owner, &r.ownerRetired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// retireStore frees the store's key. Records are kept unless the store's
// retention is delete.
func retireStore(ctx context.Context, tx pgx.Tx, storeID string) error {
	var retention string
	if err := tx.QueryRow(ctx, `UPDATE memory_stores SET retired_at = now() WHERE id = $1 RETURNING retention`, storeID).Scan(&retention); err != nil {
		return err
	}
	if retention != "delete" {
		return nil
	}
	for _, q := range []string{
		`DELETE FROM memory_revisions WHERE store_id = $1`,
		`DELETE FROM memory_records WHERE store_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, storeID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteOrganization removes an organisation's runtime. With purge=false
// (retain) the organisation is marked deleted and every seat retired so all
// capabilities are revoked while durable data stays recoverable (A19). With
// purge=true every row of the organisation is deleted.
func (s *Store) DeleteOrganization(ctx context.Context, orgID string, purge bool) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE organizations SET deleted_at = COALESCE(deleted_at, now()), updated_at = now() WHERE id = $1`, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if purge {
			return purgeOrganization(ctx, tx, orgID)
		}
		rows, err := tx.Query(ctx, `SELECT id FROM seats WHERE organization_id = $1 AND retired_at IS NULL`, orgID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := retireSeat(ctx, tx, id, "organisation deleted"); err != nil {
				return err
			}
		}
		return nil
	})
}

func purgeOrganization(ctx context.Context, tx pgx.Tx, orgID string) error {
	seatScoped := `seat_id IN (SELECT id FROM seats WHERE organization_id = $1)`
	for _, q := range []string{
		`DELETE FROM execution_events WHERE execution_id IN (SELECT e.id FROM executions e JOIN seats s ON s.id = e.seat_id WHERE s.organization_id = $1)`,
		`DELETE FROM executions WHERE ` + seatScoped,
		`DELETE FROM deliveries WHERE ` + seatScoped,
		`DELETE FROM sessions WHERE ` + seatScoped,
		`DELETE FROM handoffs WHERE ` + seatScoped,
		`DELETE FROM execution_leases WHERE ` + seatScoped,
		`DELETE FROM outbox WHERE organization_id = $1`,
		`DELETE FROM connector_operations WHERE organization_id = $1`,
		`DELETE FROM wake_schedules WHERE organization_id = $1`,
		`DELETE FROM artifacts WHERE organization_id = $1`,
		`DELETE FROM memory_revisions WHERE store_id IN (SELECT id FROM memory_stores WHERE organization_id = $1)`,
		`DELETE FROM memory_records WHERE organization_id = $1`,
		`DELETE FROM memory_stores WHERE organization_id = $1`,
		`DELETE FROM messages WHERE organization_id = $1`,
		`DELETE FROM conversations WHERE organization_id = $1`,
		`DELETE FROM seats WHERE organization_id = $1`,
		`DELETE FROM connection_checks WHERE organization_id = $1`,
		`DELETE FROM organizations WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, orgID); err != nil {
			return fmt.Errorf("purge: %w", err)
		}
	}
	return nil
}

// Organization is an active organisation and its effective manifest.
type Organization struct {
	ID        string
	Key       string
	Namespace string
	Manifest  compile.Manifest
}

// Organization loads a non-deleted organisation.
func (s *Store) Organization(ctx context.Context, orgID string) (*Organization, error) {
	o := &Organization{}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT id, key, namespace, manifest FROM organizations WHERE id = $1 AND deleted_at IS NULL`, orgID).
		Scan(&o.ID, &o.Key, &o.Namespace, &raw)
	if err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(raw, &o.Manifest); err != nil {
		return nil, err
	}
	return o, nil
}

// ActiveOrganizations lists the ids of all non-deleted organisations.
func (s *Store) ActiveOrganizations(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM organizations WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
