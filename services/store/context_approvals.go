package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func contextReviewer(owner, reviewer *Seat, org *compile.Manifest) bool {
	if owner.ID == reviewer.ID || owner.OrganizationID != reviewer.OrganizationID || owner.Manifest.Context == nil {
		return false
	}
	// Each selector retains the exact team policy that authorized it. Sharing
	// some unrelated team must never activate another team's reviewer grant.
	for _, team := range owner.Manifest.Teams {
		p := org.Spec.Teams[team].Context
		if p == nil || !p.EditableInstructions || !p.RequireInstructionApproval || !slices.Contains(reviewer.Manifest.Teams, team) {
			continue
		}
		if slices.ContainsFunc(p.InstructionApprovers, func(subject string) bool { return policy.MatchesSubject(&reviewer.Manifest, subject) }) {
			return true
		}
	}
	return false
}
func contextOrganizationManifest(ctx context.Context, tx pgx.Tx, orgID string) (*compile.Manifest, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT manifest FROM organizations WHERE id=$1 AND deleted_at IS NULL`, orgID).Scan(&raw); err != nil {
		return nil, notFound(err)
	}
	var org compile.Manifest
	if err := json.Unmarshal(raw, &org); err != nil {
		return nil, err
	}
	return &org, nil
}

// The organization lock serializes policy/config changes and proposal decisions.
// Hash full committed manifests rather than trusting only grant revision counters.
func contextPolicyToken(ctx context.Context, tx pgx.Tx, orgID string, owner *Seat) (string, error) {
	var org []byte
	if err := tx.QueryRow(ctx, `SELECT manifest FROM organizations WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, orgID).Scan(&org); err != nil {
		return "", notFound(err)
	}
	raw, err := json.Marshal(owner.Manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append(append(org, 0), raw...), []byte(fmt.Sprintf("\x00%s\x00%d", owner.ConfigRevision, owner.PolicyRevision))...))
	return hex.EncodeToString(sum[:]), nil
}
func lockContextOrganization(ctx context.Context, tx pgx.Tx, orgID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, orgID).Scan(&id)
	return notFound(err)
}
func lockedContextSeat(ctx context.Context, tx pgx.Tx, orgID, id string) (*Seat, error) {
	return scanSeat(tx.QueryRow(ctx, seatQuery+` AND s.organization_id=$1 AND s.id=$2 FOR UPDATE OF s`, orgID, id))
}
func currentContextRevision(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var revision int
	err := tx.QueryRow(ctx, `SELECT revision FROM participant_context WHERE seat_id=$1`, id).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func (s *Store) ProposeInstruction(ctx context.Context, f Fence, orgID, text, reason string, expected int) (runtimeapi.InstructionProposal, error) {
	var out runtimeapi.InstructionProposal
	if expected < 0 || len(text) > runtimeapi.MaxParticipantContextBytes || len(reason) > 1024 || !utf8.ValidString(text) || !utf8.ValidString(reason) {
		return out, fmt.Errorf("%w: invalid text, reason or revision", ErrInvalid)
	}
	encoded, _ := json.Marshal([]string{text, reason})
	if len(encoded) > 14<<10 {
		return out, fmt.Errorf("%w: escaped proposal text and reason exceed 14336 bytes", ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockContextOrganization(ctx, tx, orgID); err != nil {
			return err
		}
		owner, err := lockedContextSeat(ctx, tx, orgID, f.SeatID)
		if err != nil {
			return err
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		p := owner.Manifest.Context
		if p == nil || !p.EditableInstructions || !p.RequireInstructionApproval {
			return fmt.Errorf("%w: reviewed instruction editing is not enabled", ErrInvalid)
		}
		revision, err := currentContextRevision(ctx, tx, f.SeatID)
		if err != nil {
			return err
		}
		if revision != expected {
			return fmt.Errorf("%w: current context revision is %d", ErrConflict, revision)
		}
		token, err := contextPolicyToken(ctx, tx, orgID, owner)
		if err != nil {
			return err
		}
		// Stale proposals cannot consume the bounded pending queue indefinitely.
		if _, err := tx.Exec(ctx, `UPDATE context_instruction_proposals SET status='rejected',decided_at=now() WHERE owner_seat_id=$1 AND status='pending' AND (expected_context_revision<>$2 OR policy_token<>$3)`, f.SeatID, expected, token); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM context_instruction_proposals WHERE owner_seat_id=$1 AND status='pending'`, f.SeatID).Scan(&count); err != nil {
			return err
		}
		if count >= 8 {
			return fmt.Errorf("%w: at most eight pending instruction proposals", ErrConflict)
		}
		out = runtimeapi.InstructionProposal{ID: uuid.NewString(), OwnerSeatID: f.SeatID, Instructions: text, Reason: reason, ExpectedContextRevision: expected, PolicyToken: token, Status: "pending"}
		return tx.QueryRow(ctx, `INSERT INTO context_instruction_proposals(id,organization_id,owner_seat_id,instructions,reason,expected_context_revision,policy_token) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING sequence,created_at`, out.ID, orgID, f.SeatID, text, reason, expected, token).Scan(&out.Sequence, &out.CreatedAt)
	})
	return out, err
}

func (s *Store) DecideInstruction(ctx context.Context, f Fence, orgID, id, token string, expected int, approve bool) (runtimeapi.InstructionProposal, error) {
	var out runtimeapi.InstructionProposal
	if _, err := uuid.Parse(id); err != nil {
		return out, ErrNotFound
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockContextOrganization(ctx, tx, orgID); err != nil {
			return err
		}
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_seat_id FROM context_instruction_proposals WHERE id=$1 AND organization_id=$2`, id, orgID).Scan(&ownerID); err != nil {
			return notFound(err)
		}
		owner, err := lockedContextSeat(ctx, tx, orgID, ownerID)
		if err != nil {
			return err
		}
		reviewer, err := lockedContextSeat(ctx, tx, orgID, f.SeatID)
		if err != nil {
			return err
		}
		org, err := contextOrganizationManifest(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if !contextReviewer(owner, reviewer, org) {
			return ErrNotFound
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		out, err = scanInstructionProposal(tx.QueryRow(ctx, proposalSelect+` WHERE id=$1 AND organization_id=$2 FOR UPDATE`, id, orgID))
		if err != nil {
			return err
		}
		if out.Status != "pending" {
			return fmt.Errorf("%w: proposal already decided", ErrConflict)
		}
		currentToken, err := contextPolicyToken(ctx, tx, orgID, owner)
		if err != nil {
			return err
		}
		revision, err := currentContextRevision(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		p := owner.Manifest.Context
		if token != out.PolicyToken || currentToken != out.PolicyToken || expected != out.ExpectedContextRevision || revision != expected || p == nil || !p.EditableInstructions || !p.RequireInstructionApproval {
			return fmt.Errorf("%w: context or operator policy changed; propose again", ErrConflict)
		}
		out.Status = "rejected"
		out.ReviewerSeatID = f.SeatID
		if approve {
			var summary string
			err := tx.QueryRow(ctx, `SELECT summary FROM participant_context WHERE seat_id=$1`, ownerID).Scan(&summary)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			encoded, _ := json.Marshal([]string{out.Instructions, summary})
			if len(encoded) > 14<<10 {
				return fmt.Errorf("%w: escaped context exceeds 14336 bytes", ErrInvalid)
			}
			_, err = tx.Exec(ctx, `INSERT INTO participant_context(seat_id,revision,instructions,summary,actor_seat_id,approver_seat_id) VALUES($1,$2,$3,$4,$1,$5) ON CONFLICT(seat_id) DO UPDATE SET revision=EXCLUDED.revision,instructions=EXCLUDED.instructions,summary=EXCLUDED.summary,actor_seat_id=EXCLUDED.actor_seat_id,approver_seat_id=EXCLUDED.approver_seat_id,updated_at=clock_timestamp()`, ownerID, revision+1, out.Instructions, summary, f.SeatID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO participant_context_history SELECT * FROM participant_context WHERE seat_id=$1`, ownerID); err != nil {
				return err
			}
			out.Status = "approved"
		}
		_, err = tx.Exec(ctx, `UPDATE context_instruction_proposals SET status=$2,reviewer_seat_id=$3,decided_at=now() WHERE id=$1`, id, out.Status, f.SeatID)
		return err
	})
	return out, err
}

const proposalSelect = `SELECT id,sequence,owner_seat_id,instructions,reason,expected_context_revision,policy_token,status,COALESCE(reviewer_seat_id::text,''),created_at FROM context_instruction_proposals`

func scanInstructionProposal(row pgx.Row) (runtimeapi.InstructionProposal, error) {
	var p runtimeapi.InstructionProposal
	err := row.Scan(&p.ID, &p.Sequence, &p.OwnerSeatID, &p.Instructions, &p.Reason, &p.ExpectedContextRevision, &p.PolicyToken, &p.Status, &p.ReviewerSeatID, &p.CreatedAt)
	return p, notFound(err)
}

// One item per page bounds results and only exposes proposed text. Cursor
// advances over filtered rows so private proposals neither leak nor stall paging.
func (s *Store) InstructionProposals(ctx context.Context, orgID, callerID string, before int64) ([]runtimeapi.InstructionProposal, int64, error) {
	caller, err := s.Seat(ctx, callerID)
	if err != nil {
		return nil, 0, err
	}
	if caller.OrganizationID != orgID {
		return nil, 0, ErrNotFound
	}
	org, err := s.Organization(ctx, orgID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, proposalSelect+` WHERE organization_id=$1 AND ($2::bigint=0 OR sequence<$2) ORDER BY sequence DESC LIMIT 100`, orgID, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []runtimeapi.InstructionProposal{}
	var next int64
	// Close rows before extra reads to work with small connection pools.
	var candidates []runtimeapi.InstructionProposal
	for rows.Next() {
		p, err := scanInstructionProposal(rows)
		if err != nil {
			return nil, 0, err
		}
		candidates = append(candidates, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	for _, p := range candidates {
		next = p.Sequence
		if p.OwnerSeatID == callerID {
			out = append(out, p)
			return out, next, nil
		}
		if p.Status != "pending" {
			continue
		}
		owner, err := s.Seat(ctx, p.OwnerSeatID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if contextReviewer(owner, caller, &org.Manifest) {
			out = append(out, p)
			return out, next, nil
		}
	}
	if len(candidates) < 100 {
		next = 0
	}
	return out, next, nil
}
