package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TeamPeer deliberately omits private prompts, memory, capabilities and profiles.
type TeamPeer struct {
	Seat        string `json:"seat"`
	DisplayName string `json:"display_name"`
	CanMessage  bool   `json:"can_message"`
	CanReply    bool   `json:"can_reply"`
	Draining    bool   `json:"draining"`
}

// TeamPeers includes runtime participants as well as declarative seats. Membership
// is checked in the committed database state, not against caller-supplied keys.
func (s *Store) TeamPeers(ctx context.Context, seatID, team, after string, limit int) ([]TeamPeer, error) {
	if limit < 1 || limit > 50 {
		return nil, ErrInvalid
	}
	caller, err := s.Seat(ctx, seatID)
	if err != nil {
		return nil, err
	}
	org, err := s.Organization(ctx, caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	if _, ok := policy.TeamCoordination(&caller.Manifest, &org.Manifest, team); !ok {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT key, manifest, retiring_at IS NOT NULL FROM seats WHERE organization_id=$1 AND retired_at IS NULL AND manifest->'teams' ? $2 AND key > $3 ORDER BY key LIMIT $4`, caller.OrganizationID, team, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamPeer{}
	for rows.Next() {
		var p TeamPeer
		var raw []byte
		if err := rows.Scan(&p.Seat, &raw, &p.Draining); err != nil {
			return nil, err
		}
		var sm compile.SeatManifest
		if err := json.Unmarshal(raw, &sm); err != nil {
			return nil, err
		}
		p.DisplayName = sm.DisplayName
		edge, ok := policy.SendEdge(&caller.Manifest, p.Seat)
		p.CanMessage = ok && !p.Draining
		p.CanReply = ok && edge.Reply
		out = append(out, p)
	}
	return out, rows.Err()
}

type TeamMission struct {
	Team           string    `json:"team"`
	PolicyRevision string    `json:"policy_revision"`
	Revision       int64     `json:"revision"`
	Mission        string    `json:"mission"`
	Author         string    `json:"author,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

func (s *Store) TeamMission(ctx context.Context, seatID, team string) (*TeamMission, error) {
	caller, err := s.Seat(ctx, seatID)
	if err != nil {
		return nil, err
	}
	org, err := s.Organization(ctx, caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	p, ok := policy.TeamCoordination(&caller.Manifest, &org.Manifest, team)
	if !ok {
		return nil, ErrNotFound
	}
	out := &TeamMission{Team: team, Mission: p.Mission, PolicyRevision: org.Manifest.Digest}
	var baseline string
	err = s.pool.QueryRow(ctx, `SELECT m.revision,m.policy_mission,m.mission,a.key,m.reason,m.created_at FROM team_missions m JOIN seats a ON a.id=m.author_seat_id WHERE m.organization_id=$1 AND m.team_key=$2 ORDER BY m.revision DESC LIMIT 1`, caller.OrganizationID, team).Scan(&out.Revision, &baseline, &out.Mission, &out.Author, &out.Reason, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &TeamMission{Team: team, Mission: p.Mission, PolicyRevision: org.Manifest.Digest}, nil
	}
	if err != nil {
		return nil, err
	}
	// An operator's changed mission supersedes the runtime refinement. Retain
	// the numeric history token; PolicyRevision independently fences stale clients.
	if baseline != p.Mission {
		out.Mission, out.Author, out.Reason = p.Mission, "", "operator policy changed"
	}
	return out, nil
}

func (s *Store) UpdateTeamMission(ctx context.Context, f Fence, team string, expected int64, expectedPolicy, mission, reason string) (*TeamMission, error) {
	if expectedPolicy == "" || expected < 0 || strings.TrimSpace(mission) == "" || strings.TrimSpace(reason) == "" || len(mission) > 8192 || len(reason) > 4096 {
		return nil, ErrInvalid
	}
	var out *TeamMission
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Organization lock serializes policy updates and mission edits.
		var orgID string
		var orgRaw, seatRaw []byte
		err := tx.QueryRow(ctx, `SELECT o.id,o.manifest,s.manifest FROM organizations o JOIN seats s ON s.organization_id=o.id WHERE s.id=$1 AND s.retired_at IS NULL AND s.retiring_at IS NULL AND o.deleted_at IS NULL FOR UPDATE OF o`, f.SeatID).Scan(&orgID, &orgRaw, &seatRaw)
		if err != nil {
			return notFound(err)
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var org compile.Manifest
		var sm compile.SeatManifest
		if err := json.Unmarshal(orgRaw, &org); err != nil {
			return err
		}
		if err := json.Unmarshal(seatRaw, &sm); err != nil {
			return err
		}
		if org.Digest != expectedPolicy {
			return fmt.Errorf("%w: team policy changed; read teams.mission again", ErrConflict)
		}
		if !policy.CanEditMission(&sm, &org, team) {
			return ErrNotFound
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM team_missions WHERE organization_id=$1 AND team_key=$2`, orgID, team).Scan(&current); err != nil {
			return err
		}
		if current != expected {
			return &RevisionConflict{Current: int(current)}
		}
		out = &TeamMission{Team: team, PolicyRevision: org.Digest, Revision: current + 1, Mission: mission, Author: sm.Key, Reason: reason}
		return tx.QueryRow(ctx, `INSERT INTO team_missions(organization_id,team_key,revision,policy_mission,mission,author_seat_id,reason) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, orgID, team, out.Revision, org.Spec.Teams[team].Coordination.Mission, mission, f.SeatID, reason).Scan(&out.UpdatedAt)
	})
	return out, err
}

// checkTeamMessageBudget serializes send admission with a transaction-scoped
// advisory lock, without inverting Ack's execution/lease lock order. It runs
// inside the message transaction, so rejected sends never consume the allowance.
func checkTeamMessageBudget(ctx context.Context, tx pgx.Tx, f Fence, in SeatMessage) error {
	var seatRaw, orgRaw []byte
	if err := tx.QueryRow(ctx, `SELECT s.manifest,o.manifest FROM seats s JOIN organizations o ON o.id=s.organization_id WHERE s.id=$1 AND s.organization_id=$2 AND s.retired_at IS NULL AND o.deleted_at IS NULL`, f.SeatID, in.OrganizationID).Scan(&seatRaw, &orgRaw); err != nil {
		return notFound(err)
	}
	var sm compile.SeatManifest
	var org compile.Manifest
	if err := json.Unmarshal(seatRaw, &sm); err != nil {
		return err
	}
	if err := json.Unmarshal(orgRaw, &org); err != nil {
		return err
	}
	limit := policy.MessageLimit(&sm, &org)
	if limit == 0 && in.ExecutionID == "" {
		return nil
	}
	if in.ExecutionID == "" {
		return fmt.Errorf("%w: a current execution is required for coordinated messages", ErrInvalid)
	}
	if _, err := uuid.Parse(in.ExecutionID); err != nil {
		return fmt.Errorf("%w: invalid execution identifier", ErrInvalid)
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM executions WHERE id=$1 AND seat_id=$2 AND lease_generation=$3 AND state='running'`, in.ExecutionID, f.SeatID, f.Generation).Scan(&id); err != nil {
		return ErrFenced
	}
	if limit == 0 {
		return nil
	}
	// Ack updates executions before taking the lease; never row-lock the
	// execution while already holding the lease's share lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 42))`, "team-message:"+id); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE execution_id=$1`, id).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return fmt.Errorf("%w: per-execution team message limit reached", ErrConflict)
	}
	// Recheck the route at write time too, including reply-only edges.
	var recipient string
	if err := tx.QueryRow(ctx, `SELECT key FROM seats WHERE id=$1 AND organization_id=$2 AND retired_at IS NULL`, in.RecipientID, in.OrganizationID).Scan(&recipient); err != nil {
		return notFound(err)
	}
	if in.Route == "reply" {
		if !policy.CanReplyToSeat(&sm, recipient) {
			return ErrNotFound
		}
	} else if _, ok := policy.SendEdge(&sm, recipient); !ok {
		return ErrNotFound
	}
	// A membership-only peer relationship must never leak across teams; explicit
	// configured external routes are still honored by the existing route policy.
	return nil
}
