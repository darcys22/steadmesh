package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Graceful retirement (§5.5). A seat removed from the declaration is first
// retiring: it accepts no new messages, its queued messages go back to their
// senders, and it gets one retirement notice turn to finish what it is doing,
// save a handoff and hand over its obligations. When that turn is done, or
// the grace period ends, the seat is retired: what it still holds is handed
// back (owned work items are released, undelivered messages returned to their
// senders), the organisation's representatives get a summary, and its lease,
// schedules and personal memory are retired with every row retained.

// DefaultRetirementGrace bounds a retiring seat's wind-down.
const DefaultRetirementGrace = 10 * time.Minute

// ErrRecipientRetiring means the recipient seat is retiring or retired and
// accepts no new messages.
var ErrRecipientRetiring = errors.New("recipient seat is retiring")

// Delivery errors recorded on messages handed back by a retiring seat.
const (
	returnedRetiring = "not delivered: the recipient seat is retiring"
	returnedRetired  = "not delivered: the recipient seat retired"
)

const retireSnippet = 300

// beginRetirement marks a seat removed from the manifest as retiring, returns
// its queued messages to their senders and queues its retirement notice. It
// is idempotent: a seat already retiring keeps its deadline.
func beginRetirement(ctx context.Context, tx pgx.Tx, orgID, key string, seat *activeSeat, grace time.Duration) (runtimeapi.RetiringSeat, error) {
	out := runtimeapi.RetiringSeat{SeatID: seat.id}
	if seat.retireBy != nil {
		out.RetireBy = *seat.retireBy
		return out, nil
	}
	if grace < 0 {
		grace = 0
	}
	if err := tx.QueryRow(ctx, `UPDATE seats SET retiring_at = now(), retire_by = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 RETURNING retire_by`, seat.id, grace.Seconds()).Scan(&out.RetireBy); err != nil {
		return out, fmt.Errorf("retire seat %s: %w", key, err)
	}
	returned, err := returnDeliveries(ctx, tx, orgID, seat.id, key, []string{"pending", "passive"}, returnedRetiring)
	if err != nil {
		return out, err
	}
	noticeID, _, err := systemMessage(ctx, tx, orgID, seat.id, OriginSystem, "", retirementNotice(key, out.RetireBy, grace, returned))
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `UPDATE seats SET retire_notice_id = $2 WHERE id = $1`, seat.id, noticeID)
	return out, err
}

// cancelRetirement keeps a retiring seat that was declared again. Messages
// already returned stay returned; a notice the seat has not seen yet is
// withdrawn, otherwise the seat is told it carries on.
func cancelRetirement(ctx context.Context, tx pgx.Tx, orgID, seatID string) error {
	var notice *string
	if err := tx.QueryRow(ctx, `SELECT retire_notice_id::text FROM seats WHERE id = $1`, seatID).Scan(&notice); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE seats SET retiring_at = NULL, retire_by = NULL, retire_notice_id = NULL, updated_at = now()
		WHERE id = $1`, seatID); err != nil {
		return err
	}
	if notice == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', last_error = 'retirement cancelled', updated_at = now()
		WHERE message_id = $1 AND seat_id = $2 AND state = 'pending'`, *notice, seatID)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	_, _, err = systemMessage(ctx, tx, orgID, seatID, OriginSystem, "",
		"Retirement cancelled: this seat is in the organisation's declaration again. Carry on with your work. "+
			"Messages already returned to their senders and work items you released stay that way; check work.list and your handoff.")
	return err
}

func retirementNotice(key string, by time.Time, grace time.Duration, returned []returnedMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Retirement notice: seat %s was removed from the organisation's declaration and is retiring. "+
		"It accepts no new messages. You have until %s (%s from now) to wind down; after that your turn is interrupted and the seat stops.\n\n",
		key, by.UTC().Format(time.RFC3339), grace.Round(time.Second))
	b.WriteString(`In this turn:
1. Finish or safely pause what you are doing. Do not start anything new.
2. Save a handoff with handoff.update: the objective, what is done, what remains, and where things are (memory paths, branches, links). It is sent to the organisation's representatives, and a seat that later adopts this one starts from it.
3. Hand over your obligations. For each work item you own (work.list with owner "me"), record progress with work.update, then release it with work.release or agree a new owner with a teammate.
4. Tell the seats you were working with, with messages.send, what changed and who to ask now.

When the seat retires, whatever it still holds is handed back: owned work items are released to ready with a note pointing at your handoff, undelivered messages go back to their senders, and the organisation's representatives get a summary.
`)
	if len(returned) > 0 {
		b.WriteString("\nThese messages were waiting for you and have been returned to their senders as not delivered:\n")
		for _, m := range returned {
			fmt.Fprintf(&b, "- from %s at %s: %s\n", m.from(), m.at.UTC().Format(time.RFC3339), snippet(m.body, 200))
		}
	}
	return b.String()
}

type returnedMessage struct {
	id, origin, senderKey, senderID, binding, body string
	at                                             time.Time
}

func (m returnedMessage) from() string {
	switch {
	case m.senderKey != "":
		return "seat " + m.senderKey
	case m.binding != "":
		return "human on binding " + m.binding
	}
	return m.origin
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// returnDeliveries dead-letters the seat's deliveries in the given states with
// reason, and tells each sending seat that its message was not delivered,
// quoting it so the sender can route it elsewhere. Messages from humans and
// the platform are returned without a notice to the sender; humans are
// covered by the representatives' summary.
func returnDeliveries(ctx context.Context, tx pgx.Tx, orgID, seatID, key string, states []string, reason string) ([]returnedMessage, error) {
	rows, err := tx.Query(ctx, `WITH r AS (
			UPDATE deliveries SET state = 'dead', last_error = $3, leased_until = NULL, updated_at = now()
			WHERE seat_id = $1 AND state = ANY($2) AND message_id IS DISTINCT FROM (SELECT retire_notice_id FROM seats WHERE id = $1)
			RETURNING message_id)
		SELECT m.id, m.origin, COALESCE(ss.key, ''), COALESCE(m.sender_seat_id::text, ''), c.binding, m.body, m.created_at
		FROM r JOIN messages m ON m.id = r.message_id JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN seats ss ON ss.id = m.sender_seat_id ORDER BY m.created_at`, seatID, states, reason)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (returnedMessage, error) {
		var m returnedMessage
		err := r.Scan(&m.id, &m.origin, &m.senderKey, &m.senderID, &m.binding, &m.body, &m.at)
		return m, err
	})
	if err != nil {
		return nil, err
	}
	for _, m := range out {
		if m.origin != OriginSeat || m.senderID == "" || m.senderID == seatID {
			continue
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats WHERE id = $1 AND retiring_at IS NULL AND retired_at IS NULL)`,
			m.senderID).Scan(&live); err != nil {
			return nil, err
		}
		if !live {
			continue
		}
		body := fmt.Sprintf("Not delivered: seat %s is retiring and did not receive your message %s of %s. "+
			"Send it to whoever now handles this, or act on it yourself.\n\nYour message:\n> %s",
			key, m.id, m.at.UTC().Format(time.RFC3339), snippet(m.body, 4<<10))
		if _, _, err := systemMessage(ctx, tx, orgID, m.senderID, OriginSystem, "returned:"+m.id, body); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkRecipient refuses a message to a seat that is retiring or retired.
func checkRecipient(ctx context.Context, tx pgx.Tx, seatID string) error {
	var gone bool
	err := tx.QueryRow(ctx, `SELECT retiring_at IS NOT NULL OR retired_at IS NOT NULL FROM seats WHERE id = $1 FOR SHARE`, seatID).Scan(&gone)
	if err != nil {
		return notFound(err)
	}
	if gone {
		return ErrRecipientRetiring
	}
	return nil
}

// DueRetirement is a retiring seat ready to be retired.
type DueRetirement struct {
	OrganizationID string
	SeatID         string
	Key            string
	// Overdue means the grace period ended; otherwise the seat finished its
	// retirement turn and has nothing left in flight.
	Overdue bool
}

// DueRetirements lists retiring seats whose grace period ended, or that have
// handled their retirement notice and have no queued delivery or running
// execution left.
func (s *Store) DueRetirements(ctx context.Context, limit int) ([]DueRetirement, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.organization_id, s.id, s.key, s.retire_by <= now() FROM seats s
		WHERE s.retiring_at IS NOT NULL AND s.retired_at IS NULL AND (s.retire_by <= now() OR (
			NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.seat_id = s.id AND d.state IN ('pending', 'leased'))
			AND NOT EXISTS (SELECT 1 FROM executions e WHERE e.seat_id = s.id AND e.state = 'running')))
		ORDER BY s.retire_by LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DueRetirement, error) {
		var d DueRetirement
		err := r.Scan(&d.OrganizationID, &d.SeatID, &d.Key, &d.Overdue)
		return d, err
	})
}

// WorkRelease rewrites a work item's data for release from a retired owner,
// returning the new data and rendered body.
type WorkRelease func(data json.RawMessage) (json.RawMessage, string, error)

// Retirement reports what a finished retirement handed back.
type Retirement struct {
	OrganizationID string
	SeatID         string
	Key            string
	Overdue        bool
	// Interrupted counts executions still running at the deadline.
	Interrupted int
	// Returned are the messages returned to their senders at retirement.
	Returned int
	// ReleasedWork are the work item ids released back to ready.
	ReleasedWork []string
	// Notified are the representative seats sent the summary.
	Notified []string
}

// FinishRetirement retires a retiring seat in one transaction: running work
// is interrupted, the lease fenced, schedules stopped, undelivered messages
// returned to their senders, owned unfinished work items released through
// release, personal stores retired, and a summary sent to the organisation's
// representatives. It returns nil when the seat is not retiring any more.
func (s *Store) FinishRetirement(ctx context.Context, seatID string, release WorkRelease) (*Retirement, error) {
	var out *Retirement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		r := &Retirement{SeatID: seatID}
		var retiring bool
		var gen int64
		err := tx.QueryRow(ctx, `SELECT s.organization_id, s.key, s.retiring_at IS NOT NULL AND s.retired_at IS NULL,
				COALESCE(s.retire_by <= now(), false), COALESCE(l.generation, 0)
			FROM seats s LEFT JOIN execution_leases l ON l.seat_id = s.id WHERE s.id = $1 FOR UPDATE OF s`, seatID).
			Scan(&r.OrganizationID, &r.Key, &retiring, &r.Overdue, &gen)
		if err != nil {
			return notFound(err)
		}
		if !retiring {
			return nil
		}

		returned, err := returnDeliveries(ctx, tx, r.OrganizationID, seatID, r.Key, []string{"pending", "leased", "passive"}, returnedRetired)
		if err != nil {
			return err
		}
		reason := "seat retired"
		if r.Overdue {
			reason = "retirement grace period ended"
		}
		tag, err := tx.Exec(ctx, `UPDATE executions SET state = 'interrupted', error = $2, finished_at = now()
			WHERE seat_id = $1 AND state = 'running'`, seatID, reason)
		if err != nil {
			return err
		}
		r.Interrupted = int(tag.RowsAffected())
		if err := retireSeat(ctx, tx, seatID, reason); err != nil {
			return err
		}
		stores, err := tx.Query(ctx, `SELECT id FROM memory_stores WHERE owner_seat_id = $1 AND retired_at IS NULL`, seatID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(stores, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := retireStore(ctx, tx, id); err != nil {
				return err
			}
		}
		released, err := releaseWork(ctx, tx, r.OrganizationID, seatID, r.Key, gen, release)
		if err != nil {
			return err
		}
		for _, w := range released {
			r.ReleasedWork = append(r.ReleasedWork, w.id)
		}
		var h runtimeapi.Handoff
		var hasHandoff bool
		err = tx.QueryRow(ctx, `SELECT objective, unresolved, notes FROM handoffs WHERE seat_id = $1`, seatID).Scan(&h.Objective, &h.Unresolved, &h.Notes)
		switch {
		case err == nil:
			hasHandoff = true
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var earlier []returnedMessage
		rows, err := tx.Query(ctx, `SELECT m.id, m.origin, COALESCE(ss.key, ''), COALESCE(m.sender_seat_id::text, ''), c.binding, m.body, m.created_at
			FROM deliveries d JOIN messages m ON m.id = d.message_id JOIN conversations c ON c.id = m.conversation_id
			LEFT JOIN seats ss ON ss.id = m.sender_seat_id
			WHERE d.seat_id = $1 AND d.state = 'dead' AND d.last_error = $2 ORDER BY m.created_at`, seatID, returnedRetiring)
		if err != nil {
			return err
		}
		earlier, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (returnedMessage, error) {
			var m returnedMessage
			err := row.Scan(&m.id, &m.origin, &m.senderKey, &m.senderID, &m.binding, &m.body, &m.at)
			return m, err
		})
		if err != nil {
			return err
		}
		all := append(earlier, returned...)
		r.Returned = len(all)
		reps, err := representatives(ctx, tx, r.OrganizationID)
		if err != nil {
			return err
		}
		body := retirementSummary(r, hasHandoff, h, released, all)
		for _, rep := range reps {
			if _, _, err := systemMessage(ctx, tx, r.OrganizationID, rep.id, OriginSystem, "retired:"+seatID+":"+rep.id, body); err != nil {
				return err
			}
			r.Notified = append(r.Notified, rep.key)
		}
		out = r
		return nil
	})
	return out, err
}

type releasedWork struct{ id, objective, store string }

// releaseWork releases the unfinished work items the retired seat owned in
// the organisation's live stores, recording the release in each item's
// history under the seat's identity.
func releaseWork(ctx context.Context, tx pgx.Tx, orgID, seatID, key string, gen int64, release WorkRelease) ([]releasedWork, error) {
	if release == nil {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT r.id, r.data, ms.key FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id
		WHERE r.organization_id = $1 AND r.kind = $2 AND NOT r.archived AND ms.retired_at IS NULL AND ms.owner_seat_id IS NULL
		  AND r.data->>'owner' = $3 AND COALESCE(r.data->>'status', '') NOT IN ('done', 'cancelled')
		ORDER BY r.path FOR UPDATE OF r`, orgID, KindWork, key)
	if err != nil {
		return nil, err
	}
	type item struct {
		id, store string
		data      json.RawMessage
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		err := r.Scan(&it.id, &it.data, &it.store)
		return it, err
	})
	if err != nil {
		return nil, err
	}
	var out []releasedWork
	for _, it := range items {
		data, body, err := release(it.data)
		if err != nil {
			return nil, fmt.Errorf("release work record %s: %w", it.id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, data = $2, body = $3, author_seat_id = $4,
			updated_at = now() WHERE id = $1`, it.id, data, body, seatID); err != nil {
			return nil, err
		}
		if err := appendRevision(ctx, tx, it.id, gen); err != nil {
			return nil, err
		}
		var w struct {
			ID        string `json:"id"`
			Objective string `json:"objective"`
		}
		_ = json.Unmarshal(data, &w)
		out = append(out, releasedWork{id: w.ID, objective: w.Objective, store: it.store})
	}
	return out, nil
}

type seatRef struct{ id, key string }

// representatives are the organisation's live representative seats.
func representatives(ctx context.Context, tx pgx.Tx, orgID string) ([]seatRef, error) {
	rows, err := tx.Query(ctx, `SELECT id, key, manifest FROM seats WHERE organization_id = $1 AND retired_at IS NULL AND retiring_at IS NULL
		ORDER BY key`, orgID)
	if err != nil {
		return nil, err
	}
	var out []seatRef
	for rows.Next() {
		var ref seatRef
		var raw []byte
		if err := rows.Scan(&ref.id, &ref.key, &raw); err != nil {
			return nil, err
		}
		var sm compile.SeatManifest
		if json.Unmarshal(raw, &sm) == nil && sm.IsRepresentative {
			out = append(out, ref)
		}
	}
	return out, rows.Err()
}

func retirementSummary(r *Retirement, hasHandoff bool, h runtimeapi.Handoff, released []releasedWork, returned []returnedMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Seat %s has retired: it was removed from the organisation's declaration. ", r.Key)
	if r.Overdue {
		b.WriteString("Its grace period ended before it finished winding down")
		if r.Interrupted > 0 {
			b.WriteString(", and its last turn was interrupted")
		}
		b.WriteString(".\n")
	} else {
		b.WriteString("It finished its retirement turn.\n")
	}
	if hasHandoff && (h.Objective != "" || h.Notes != "" || len(h.Unresolved) > 0) {
		b.WriteString("\nIts handoff:\n")
		if h.Objective != "" {
			fmt.Fprintf(&b, "- objective: %s\n", snippet(h.Objective, 600))
		}
		for _, u := range h.Unresolved {
			fmt.Fprintf(&b, "- unresolved: %s\n", snippet(u, 300))
		}
		if h.Notes != "" {
			fmt.Fprintf(&b, "- notes: %s\n", snippet(h.Notes, 1500))
		}
	} else {
		b.WriteString("\nIt left no handoff.\n")
	}
	if len(released) > 0 {
		b.WriteString("\nWork items it owned, released back to ready for a new owner:\n")
		for _, w := range released {
			fmt.Fprintf(&b, "- %s (%s): %s\n", w.id, w.store, snippet(w.objective, retireSnippet))
		}
	}
	if len(returned) > 0 {
		b.WriteString("\nMessages it never handled, returned as not delivered (seat senders were told):\n")
		for _, m := range returned {
			fmt.Fprintf(&b, "- from %s at %s: %s\n", m.from(), m.at.UTC().Format(time.RFC3339), snippet(m.body, retireSnippet))
		}
	}
	b.WriteString("\nDecide who picks up what is left, and tell the people who were waiting on this seat.")
	return b.String()
}

// RetiringManifests returns the committed manifests of the organisation's
// retiring seats, by key: they keep their access while winding down.
func (s *Store) RetiringManifests(ctx context.Context, orgID string) (map[string]compile.SeatManifest, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, manifest FROM seats WHERE organization_id = $1 AND retiring_at IS NOT NULL AND retired_at IS NULL`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]compile.SeatManifest{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var sm compile.SeatManifest
		if err := json.Unmarshal(raw, &sm); err != nil {
			return nil, err
		}
		out[key] = sm
	}
	return out, rows.Err()
}
