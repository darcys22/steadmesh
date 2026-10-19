package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/jackc/pgx/v5"
)

// ParticipantContext reads only this identity's context; no parent fallback.
// revision zero selects the current version, and an absent current row is v0.
func (s *Store) ParticipantContext(ctx context.Context, orgID, seatID string, revision int) (runtimeapi.ParticipantContext, error) {
	var out runtimeapi.ParticipantContext
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM seats WHERE id=$1 AND organization_id=$2)`, seatID, orgID).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}
	table := "participant_context"
	args := []any{seatID}
	clause := "seat_id=$1"
	if revision > 0 {
		table = "participant_context_history"
		clause += " AND revision=$2"
		args = append(args, revision)
	}
	err := s.pool.QueryRow(ctx, `SELECT revision,instructions,summary,actor_seat_id,updated_at,COALESCE(approver_seat_id::text,'') FROM `+table+` WHERE `+clause, args...).Scan(&out.Revision, &out.Instructions, &out.Summary, &out.ActorSeatID, &out.UpdatedAt, &out.ApproverSeatID)
	if errors.Is(err, pgx.ErrNoRows) {
		if revision == 0 {
			return out, nil
		}
		return out, ErrNotFound
	}
	return out, err
}

// UpdateParticipantContext changes one field, or restores that field from a
// historical revision. ExpectedRevision is mandatory, including zero on create.
// Authority is re-read while holding the same seat row lock as policy updates.
func (s *Store) UpdateParticipantContext(ctx context.Context, f Fence, orgID, field, text string, expectedRevision, restoreRevision int) (runtimeapi.ParticipantContext, error) {
	var out runtimeapi.ParticipantContext
	if (field != "instructions" && field != "summary") || expectedRevision < 0 || restoreRevision < 0 || len(text) > runtimeapi.MaxParticipantContextBytes || !utf8.ValidString(text) {
		return out, fmt.Errorf("%w: invalid context field, revision or text", ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT manifest FROM seats WHERE id=$1 AND organization_id=$2 AND retired_at IS NULL FOR UPDATE`, f.SeatID, orgID).Scan(&raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var sm compile.SeatManifest
		if err := json.Unmarshal(raw, &sm); err != nil {
			return err
		}
		if sm.Context == nil || (field == "instructions" && !sm.Context.EditableInstructions) || (field == "summary" && !sm.Context.EditableSummary) {
			return fmt.Errorf("%w: context field is not editable", ErrInvalid)
		}
		if field == "instructions" && sm.Context.RequireInstructionApproval {
			return fmt.Errorf("%w: instruction changes require a reviewed proposal", ErrInvalid)
		}
		err := tx.QueryRow(ctx, `SELECT revision,instructions,summary FROM participant_context WHERE seat_id=$1`, f.SeatID).Scan(&out.Revision, &out.Instructions, &out.Summary)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if out.Revision != expectedRevision {
			return fmt.Errorf("%w: current context revision is %d", ErrConflict, out.Revision)
		}
		if restoreRevision > 0 {
			var instructions, summary string
			if err := tx.QueryRow(ctx, `SELECT instructions,summary FROM participant_context_history WHERE seat_id=$1 AND revision=$2`, f.SeatID, restoreRevision).Scan(&instructions, &summary); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			if field == "instructions" {
				text = instructions
			} else {
				text = summary
			}
		}
		if field == "instructions" {
			out.Instructions = text
		} else {
			out.Summary = text
		}
		// Leave room for attribution and timestamps inside the tool result
		// bound, even when JSON expands control characters or angle brackets.
		encoded, err := json.Marshal([]string{out.Instructions, out.Summary})
		if err != nil {
			return err
		}
		if len(encoded) > 14<<10 {
			return fmt.Errorf("%w: escaped context exceeds 14336 bytes; shorten the text", ErrInvalid)
		}
		out.Revision++
		out.ActorSeatID = f.SeatID
		if err := tx.QueryRow(ctx, `INSERT INTO participant_context (seat_id,revision,instructions,summary,actor_seat_id) VALUES ($1,$2,$3,$4,$1) ON CONFLICT (seat_id) DO UPDATE SET revision=EXCLUDED.revision,instructions=EXCLUDED.instructions,summary=EXCLUDED.summary,actor_seat_id=EXCLUDED.actor_seat_id,updated_at=clock_timestamp(),approver_seat_id=NULL RETURNING updated_at`, f.SeatID, out.Revision, out.Instructions, out.Summary).Scan(&out.UpdatedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO participant_context_history SELECT * FROM participant_context WHERE seat_id=$1`, f.SeatID)
		return err
	})
	return out, err
}

// ParticipantContextHistory omits content to keep bounded pages small.
func (s *Store) ParticipantContextHistory(ctx context.Context, orgID, seatID string, before, limit int) ([]runtimeapi.ParticipantContext, error) {
	if limit < 1 || limit > 50 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT h.revision,h.actor_seat_id,h.updated_at,COALESCE(h.approver_seat_id::text,'') FROM participant_context_history h JOIN seats s ON s.id=h.seat_id WHERE s.id=$1 AND s.organization_id=$2 AND ($3::integer=0 OR h.revision<$3) ORDER BY h.revision DESC LIMIT $4`, seatID, orgID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []runtimeapi.ParticipantContext{}
	for rows.Next() {
		var v runtimeapi.ParticipantContext
		if err := rows.Scan(&v.Revision, &v.ActorSeatID, &v.UpdatedAt, &v.ApproverSeatID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
