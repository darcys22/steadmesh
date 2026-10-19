package runtimeapi

import "time"

// ParticipantContext is private, agent-authored working context. It never
// replaces operator instructions, mission, policy, identity or grants.
type ParticipantContext struct {
	ApproverSeatID string    `json:"approver_seat_id,omitempty"`
	Revision       int       `json:"revision"`
	Instructions   string    `json:"instructions"`
	Summary        string    `json:"summary"`
	ActorSeatID    string    `json:"actor_seat_id,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

const MaxParticipantContextBytes = 4096 // per editable field, measured in bytes

// InstructionProposal exposes proposed text, not the owner's private context.
type InstructionProposal struct {
	ID                      string    `json:"id"`
	Sequence                int64     `json:"sequence"`
	OwnerSeatID             string    `json:"owner_seat_id"`
	Instructions            string    `json:"instructions"`
	Reason                  string    `json:"reason"`
	ExpectedContextRevision int       `json:"expected_context_revision"`
	PolicyToken             string    `json:"policy_token"`
	Status                  string    `json:"status"`
	ReviewerSeatID          string    `json:"reviewer_seat_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}
