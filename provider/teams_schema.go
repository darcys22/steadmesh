package provider

import "github.com/hashicorp/terraform-plugin-framework/resource/schema"

func teamContextAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{Optional: true, Description: "Independent self-context editing permissions. Team memberships union permissions; these never grant memory access.", Attributes: map[string]schema.Attribute{
		"require_instruction_approval": optBool("Require reviewed proposals for instruction edits. Requires editable_instructions and explicit instruction_approvers. Any team approval requirement wins across memberships."),
		"instruction_approvers":        strList("Explicit seat:<member-key> or team:<this-team> reviewers. Reviewers union across team memberships; approval grants no other authority."),
		"editable_instructions":        optBool("Allow members to edit only their own versioned instructions, subject to require_instruction_approval. Default false."),
		"editable_summary":             optBool("Allow members to edit only their own versioned summary. Default false."),
	}}
}
func teamCoordinationAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{Optional: true, Description: "Coordination policy, independent of pooling, tools and message authority.", Attributes: map[string]schema.Attribute{
		"mode":                       optStr("declared (default) or self_organising. Neither grants routes or tools."),
		"mission":                    optStr("Initial mission, at most 8192 UTF-8 bytes."),
		"work_store":                 optStr("Unique shared work board store. Requires explicit team read, write and revise grants."),
		"mission_editors":            strList("Explicit seat:<member-key> or team:<this-team> selectors. Empty grants no agent mission-edit permission."),
		"review_policy":              optStr("none (default) or required. Required review needs a work_store."),
		"max_messages_per_execution": optInt("Bounded coordination messages per execution, 1–1000; default 20."),
	}}
}
func teamPoolAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{Optional: true, Description: "Native participant pool with independent seat identities. Runtime membership never changes declared seats.", Attributes: map[string]schema.Attribute{
		"min_members":           optInt("Minimum participants, 0–1024, default 0; cannot exceed max_members."),
		"max_members":           schema.Int64Attribute{Required: true, Description: "Maximum active participants, 1–1024."},
		"idle_timeout_seconds":  optInt("Idle retirement bound, 1–86400 seconds; default 900."),
		"drain_timeout_seconds": optInt("Graceful drain bound, 1–3600 seconds; default 300."),
		"retention_seconds":     optInt("Retired private-data retention. Zero (default) retains indefinitely; 1–2592000 seconds explicitly opts into automatic cleanup after retirement."),
		"draining":              optBool("Stop admission while existing members drain. Default false."),
		"templates": schema.MapNestedAttribute{Required: true, Description: "Explicit participant templates. Owning team membership is automatically included; all authority uses ordinary team/seat grants and access profiles.", NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"instructions":      reqStr("Task instructions, nonempty UTF-8, at most 8192 bytes."),
			"seat":              schema.SingleNestedAttribute{Required: true, Description: "Independent seat configuration. Fixed personal_memory and adopt_from are forbidden; private memory is runtime allocated.", Attributes: seatAttributes()},
			"tool_names":        strList("Legacy only; must be omitted in native pools."),
			"harness_profile":   optStr("Legacy only; use seat.harness_profile."),
			"execution_profile": optStr("Legacy only; use seat.execution_profile."),
		}}},
	}}
}
