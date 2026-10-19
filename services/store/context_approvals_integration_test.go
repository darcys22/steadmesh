//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func TestInstructionApprovalWorkflow(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := orgfixture.Spec()
	team := sp.Teams["engineering"]
	team.Context = &spec.TeamContext{EditableInstructions: true, EditableSummary: true, RequireInstructionApproval: true, InstructionApprovers: []string{"team:engineering"}}
	sp.Teams["engineering"] = team
	m := orgfixture.Compile(t, sp)
	res := syncOrg(t, s, m)
	owner := lease(t, s, res.Seats["lead"].SeatID)
	reviewer := lease(t, s, res.Seats["reviewer"].SeatID)
	outsider := lease(t, s, res.Seats["rep_a"].SeatID)
	// Both mutation routes must deny approval bypass, including restoring old text.
	for _, restore := range []int{0, 1} {
		if _, err := s.UpdateParticipantContext(ctx, owner, res.OrganizationID, "instructions", "bypass", 0, restore); !errors.Is(err, ErrInvalid) {
			t.Fatalf("direct bypass accepted: %v", err)
		}
	}
	if _, err := s.UpdateParticipantContext(ctx, owner, res.OrganizationID, "summary", "PRIVATE SUMMARY", 0, 0); err != nil {
		t.Fatal(err)
	}
	p, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "reviewed plan", "explain why", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideInstruction(ctx, owner, res.OrganizationID, p.ID, p.PolicyToken, 1, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("self approval: %v", err)
	}
	if _, err := s.DecideInstruction(ctx, outsider, res.OrganizationID, p.ID, p.PolicyToken, 1, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider approval: %v", err)
	}
	list, _, err := s.InstructionProposals(ctx, res.OrganizationID, outsider.SeatID, 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("outsider list: %+v %v", list, err)
	}
	list, _, err = s.InstructionProposals(ctx, res.OrganizationID, reviewer.SeatID, 0)
	if err != nil || len(list) != 1 || list[0].Instructions != "reviewed plan" || strings.Contains(list[0].Reason, "PRIVATE SUMMARY") {
		t.Fatalf("review list: %+v %v", list, err)
	}
	if _, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, p.ID, "wrong-token", 1, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong token: %v", err)
	}
	approved, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, p.ID, p.PolicyToken, 1, true)
	if err != nil || approved.Status != "approved" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	current, err := s.ParticipantContext(ctx, res.OrganizationID, owner.SeatID, 0)
	if err != nil || current.Revision != 2 || current.Instructions != "reviewed plan" || current.Summary != "PRIVATE SUMMARY" || current.ActorSeatID != owner.SeatID || current.ApproverSeatID != reviewer.SeatID {
		t.Fatalf("approved snapshot: %+v %v", current, err)
	}
	history, err := s.ParticipantContextHistory(ctx, res.OrganizationID, owner.SeatID, 0, 20)
	if err != nil || history[0].ApproverSeatID != reviewer.SeatID {
		t.Fatalf("approval audit: %+v %v", history, err)
	}
	if _, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, p.ID, p.PolicyToken, 1, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat approval: %v", err)
	}
	// Context changes, including a summary edit, invalidate pending approvals.
	stale, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "stale plan", "reason", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateParticipantContext(ctx, owner, res.OrganizationID, "summary", "new summary", 2, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, stale.ID, stale.PolicyToken, 2, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale context accepted: %v", err)
	}
	// Operator configuration changes invalidate approvals, even without a grant change.
	stalePolicy, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "policy stale", "reason", 3)
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.DisplayName = "Operator changed configuration"
	syncOrg(t, s, m)
	if _, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, stalePolicy.ID, stalePolicy.PolicyToken, 3, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale operator config accepted: %v", err)
	}
	reject, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "reject plan", "reason", 3)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := s.DecideInstruction(ctx, reviewer, res.OrganizationID, reject.ID, reject.PolicyToken, 3, false)
	if err != nil || rejected.Status != "rejected" {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	current, err = s.ParticipantContext(ctx, res.OrganizationID, owner.SeatID, 0)
	if err != nil || current.Revision != 3 || current.Instructions != "reviewed plan" {
		t.Fatalf("rejection changed active context: %+v %v", current, err)
	}
	// Repeated stale proposals never permanently consume the pending cap.
	for i := 0; i < 10; i++ {
		if _, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "stale queue", "reason", 3+i); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateParticipantContext(ctx, owner, res.OrganizationID, "summary", "invalidate", 3+i, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Pending queue has a hard bound; stale proposals were cleared above.
	for i := 0; i < 8; i++ {
		if _, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "bounded", "reason", 13); err != nil {
			t.Fatalf("proposal %d: %v", i, err)
		}
	}
	if _, err := s.ProposeInstruction(ctx, owner, res.OrganizationID, "overflow", "reason", 13); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending cap: %v", err)
	}
}
