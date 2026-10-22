package provider

import (
	"context"
	"fmt"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

func skillPolicyAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:    true,
		Description: "Marketplace skills for Claude Code, Codex and Pi. Omitted means disabled. Only selected skill files are loaded, never plugin hooks, commands, agents or MCP servers; skills grant no additional access.",
		Attributes: map[string]schema.Attribute{
			"mode":         optStr("explicit (default): static installs only; discover: also allow catalog listing without dynamic installation; agent_choice: allow bounded agent-selected installs from the allowlist."),
			"marketplaces": reqStrList("Nonempty allowlist of up to 32 organization skill_marketplaces keys. Every static or dynamic install must belong to one of these marketplaces."),
			"install": schema.ListNestedAttribute{
				Optional:    true,
				Description: "Up to 128 individual skills to install before execution, in every mode. A plugin and a skill are distinct identifiers; installing a skill does not enable the rest of its plugin.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"marketplace": reqStr("An allowed skill_marketplaces key."),
					"plugin":      reqStr("Plugin identifier in that marketplace: lowercase letters, digits, underscores or hyphens; 1–128 characters, starting with a letter or digit."),
					"skill":       reqStr("Individual skill identifier inside the plugin: lowercase letters, digits, underscores or hyphens; 1–128 characters, starting with a letter or digit."),
				}},
			},
			"max_dynamic_skills": schema.Int64Attribute{
				Optional:    true,
				Description: "Maximum active agent-selected skills, excluding static installs. Only valid for agent_choice; 1–128, default 16.",
				Validators:  []validator.Int64{skillLimitValidator{}},
			},
		},
	}
}

type skillLimitValidator struct{}

func (skillLimitValidator) Description(context.Context) string {
	return fmt.Sprintf("must be between 1 and %d", spec.MaxDynamicSkills)
}
func (v skillLimitValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v skillLimitValidator) ValidateInt64(ctx context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	n := req.ConfigValue.ValueInt64()
	if n < 1 || n > spec.MaxDynamicSkills {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid dynamic skill limit", v.Description(ctx))
	}
}
