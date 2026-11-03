package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func startupFallbackAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:    true,
		Description: "Explicit pre-start failover for a missing harness executable only. Never replays deliveries or switches native sessions. All profiles must use the same operator-provided multi-harness image; task workers keep the primary only.",
		Attributes: map[string]schema.Attribute{
			"profiles":      schema.ListAttribute{Required: true, ElementType: types.StringType, Validators: []validator.List{startupFallbackProfilesValidator{}}, Description: "Ordered list of 1–7 existing alternative profiles. No duplicates, self-references or nested fallback. Every alternative preserves the primary's required capabilities."},
			"max_attempts":  schema.Int64Attribute{Required: true, Validators: []validator.Int64{startupFallbackAttemptsValidator{}}, Description: "Total attempts including the primary, 1–8 and no greater than the approved chain length."},
			"total_timeout": schema.StringAttribute{Required: true, Validators: []validator.String{startupFallbackTimeoutValidator{}}, Description: "Positive Go duration at most 5m, shared by all startup attempts and additionally bounded by the parent context."},
		},
	}
}

type startupFallbackProfilesValidator struct{}

func (startupFallbackProfilesValidator) Description(context.Context) string {
	return fmt.Sprintf("must contain 1 to %d alternative profiles", harnesses.MaxFallbackAttempts-1)
}
func (v startupFallbackProfilesValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v startupFallbackProfilesValidator) ValidateList(ctx context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := len(req.ConfigValue.Elements()); n < 1 || n >= harnesses.MaxFallbackAttempts {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid startup fallback profiles", v.Description(ctx))
	}
}

type startupFallbackAttemptsValidator struct{}

func (startupFallbackAttemptsValidator) Description(context.Context) string {
	return fmt.Sprintf("must be between 1 and %d", harnesses.MaxFallbackAttempts)
}
func (v startupFallbackAttemptsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v startupFallbackAttemptsValidator) ValidateInt64(ctx context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := req.ConfigValue.ValueInt64(); n < 1 || n > harnesses.MaxFallbackAttempts {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid startup fallback attempts", v.Description(ctx))
	}
}

type startupFallbackTimeoutValidator struct{}

func (startupFallbackTimeoutValidator) Description(context.Context) string {
	return fmt.Sprintf("must be a positive duration no greater than %s", compile.MaxStartupFallbackTimeout)
}
func (v startupFallbackTimeoutValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v startupFallbackTimeoutValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if d, err := time.ParseDuration(req.ConfigValue.ValueString()); err != nil || d <= 0 || d > compile.MaxStartupFallbackTimeout {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid startup fallback timeout", v.Description(ctx))
	}
}
