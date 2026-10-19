package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func contextEditable(sm *compile.SeatManifest, _ *compile.Manifest) bool {
	return sm.Context != nil && (sm.Context.EditableInstructions || sm.Context.EditableSummary)
}

func (r *Registry) contextTools() []*tool {
	return append(r.contextApprovalTools(), []*tool{
		{name: "context.read", description: "Read your private working context (current by default, or a historical revision). This is separate from immutable operator instructions, mission and grants.", schema: `{"type":"object","properties":{"revision":{"type":"integer","minimum":1}},"additionalProperties":false}`, allowed: always, handle: r.contextRead},
		{name: "context.update", description: "Replace one permitted local working-context field, at expected_revision (0 initially). Maximum 4096 UTF-8 bytes per field and 14336 bytes for the combined JSON-escaped text. Takes effect on your next turn, including resumed sessions. Never changes operator instructions, mission, policy or memory permissions.", schema: `{"type":"object","properties":{"field":{"enum":["instructions","summary"]},"text":{"type":"string"},"expected_revision":{"type":"integer","minimum":0}},"required":["field","text","expected_revision"],"additionalProperties":false}`, allowed: contextEditable, mutating: true, handle: r.contextUpdate},
		{name: "context.rollback", description: "Restore one permitted field from a historical revision, appending a new attributed revision. Other fields remain unchanged. Takes effect next turn.", schema: `{"type":"object","properties":{"field":{"enum":["instructions","summary"]},"revision":{"type":"integer","minimum":1},"expected_revision":{"type":"integer","minimum":0}},"required":["field","revision","expected_revision"],"additionalProperties":false}`, allowed: contextEditable, mutating: true, handle: r.contextRollback},
		{name: "context.history", description: "List your private working-context revision metadata, newest first. Use context.read with a revision for its content; use the last revision as before_revision for the next page.", schema: `{"type":"object","properties":{"before_revision":{"type":"integer","minimum":1}},"additionalProperties":false}`, allowed: always, handle: r.contextHistory},
	}...)
}

func decodeContext(c *Call, v any) error {
	d := json.NewDecoder(bytes.NewReader(c.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return toolErr("invalid", "arguments: %v", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return toolErr("invalid", "expected one argument object")
	}
	return nil
}
func (r *Registry) contextRead(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Revision int `json:"revision"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if a.Revision < 0 {
		return nil, toolErr("invalid", "invalid revision")
	}
	return r.d.Store.ParticipantContext(ctx, c.Seat.OrganizationID, c.Seat.ID, a.Revision)
}
func (r *Registry) contextHistory(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Before int `json:"before_revision"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if a.Before < 0 {
		return nil, toolErr("invalid", "invalid revision")
	}
	return r.d.Store.ParticipantContextHistory(ctx, c.Seat.OrganizationID, c.Seat.ID, a.Before, 20)
}
func contextFieldAllowed(c *Call, field string) error {
	p := c.Seat.Manifest.Context
	if field != "instructions" && field != "summary" {
		return toolErr("invalid", "field must be instructions or summary")
	}
	if p == nil || (field == "instructions" && !p.EditableInstructions) || (field == "summary" && !p.EditableSummary) {
		return toolErr("forbidden", "editing %s is not permitted", field)
	}
	if field == "instructions" && p.RequireInstructionApproval {
		return toolErr("forbidden", "instruction changes require context.propose_instruction and independent approval")
	}
	return nil
}
func (r *Registry) contextUpdate(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Field    string  `json:"field"`
		Text     *string `json:"text"`
		Expected *int    `json:"expected_revision"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if err := contextFieldAllowed(c, a.Field); err != nil {
		return nil, err
	}
	if a.Expected == nil || a.Text == nil || *a.Expected < 0 || len(*a.Text) > runtimeapi.MaxParticipantContextBytes {
		return nil, toolErr("invalid", "text (at most 4096 bytes) and nonnegative expected_revision are required")
	}
	return r.d.Store.UpdateParticipantContext(ctx, c.fence(), c.Seat.OrganizationID, a.Field, *a.Text, *a.Expected, 0)
}
func (r *Registry) contextRollback(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Field    string `json:"field"`
		Revision int    `json:"revision"`
		Expected *int   `json:"expected_revision"`
	}
	if err := decodeContext(c, &a); err != nil {
		return nil, err
	}
	if err := contextFieldAllowed(c, a.Field); err != nil {
		return nil, err
	}
	if a.Expected == nil || *a.Expected < 0 || a.Revision < 1 {
		return nil, toolErr("invalid", "positive revision and nonnegative expected_revision are required")
	}
	return r.d.Store.UpdateParticipantContext(ctx, c.fence(), c.Seat.OrganizationID, a.Field, "", *a.Expected, a.Revision)
}
