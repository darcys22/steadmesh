package tools

import (
	"context"
	"github.com/darcys22/steadmesh/pkg/compile"
)

func (r *Registry) contextApprovalTools() []*tool {
	return []*tool{
		{name: "context.propose_instruction", description: "Submit your proposed local instructions for independent review. The original operator instructions remain immutable. Requires current context revision, bounded text (4096 bytes) and reason (1024 bytes); at most eight pending proposals. Does not change active context.", schema: `{"type":"object","properties":{"text":{"type":"string"},"reason":{"type":"string"},"expected_context_revision":{"type":"integer","minimum":0}},"required":["text","reason","expected_context_revision"],"additionalProperties":false}`, allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
			return sm.Context != nil && sm.Context.EditableInstructions && sm.Context.RequireInstructionApproval
		}, mutating: true, handle: r.contextProposeInstruction},
		{name: "context.proposals", description: "Read your own instruction proposals and pending proposals you are explicitly authorized to review. Only proposed text is shared, never private context. One proposal per page; pass next_before_sequence until zero.", schema: `{"type":"object","properties":{"before_sequence":{"type":"integer","minimum":1}},"additionalProperties":false}`, allowed: always, handle: r.contextProposals},
		{name: "context.approve_instruction", description: "Approve or reject a specific instruction proposal as its explicitly authorized independent same-team reviewer. Pass its exact expected_context_revision and policy_token. Approval appends an author-and-approver-attributed context revision used next turn. Changed context or policy rejects stale decisions. Self approval is never permitted.", schema: `{"type":"object","properties":{"proposal_id":{"type":"string"},"expected_context_revision":{"type":"integer","minimum":0},"policy_token":{"type":"string"},"approve":{"type":"boolean"}},"required":["proposal_id","expected_context_revision","policy_token","approve"],"additionalProperties":false}`, allowed: always, mutating: true, handle: r.contextApproveInstruction},
	}
}
func (r *Registry) contextProposeInstruction(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Text     *string `json:"text"`
		Reason   *string `json:"reason"`
		Expected *int    `json:"expected_context_revision"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if a.Text == nil || a.Reason == nil || a.Expected == nil {
		return nil, toolErr("invalid", "text, reason and expected_context_revision are required")
	}
	return r.d.Store.ProposeInstruction(ctx, c.fence(), c.Seat.OrganizationID, *a.Text, *a.Reason, *a.Expected)
}
func (r *Registry) contextProposals(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Before int64 `json:"before_sequence"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if a.Before < 0 {
		return nil, toolErr("invalid", "before_sequence must be positive")
	}
	proposals, next, err := r.d.Store.InstructionProposals(ctx, c.Seat.OrganizationID, c.Seat.ID, a.Before)
	if err != nil {
		return nil, err
	}
	return map[string]any{"proposals": proposals, "next_before_sequence": next}, nil
}
func (r *Registry) contextApproveInstruction(ctx context.Context, c *Call) (any, error) {
	var a struct {
		ID       string `json:"proposal_id"`
		Expected *int   `json:"expected_context_revision"`
		Token    string `json:"policy_token"`
		Approve  *bool  `json:"approve"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if a.ID == "" || a.Token == "" || a.Expected == nil || *a.Expected < 0 || a.Approve == nil {
		return nil, toolErr("invalid", "proposal_id, policy_token, expected_context_revision and approve are required")
	}
	return r.d.Store.DecideInstruction(ctx, c.fence(), c.Seat.OrganizationID, a.ID, a.Token, *a.Expected, *a.Approve)
}
