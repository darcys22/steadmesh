package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// Field validators still run when unrelated instruction references are unknown,
// before the compiler can validate the complete organization declaration.
type elasticLimitValidator struct{ max int64 }

func (v elasticLimitValidator) Description(context.Context) string {
	return fmt.Sprintf("must be between 1 and %d", v.max)
}
func (v elasticLimitValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v elasticLimitValidator) ValidateInt64(ctx context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	n := req.ConfigValue.ValueInt64()
	if n < 1 || n > v.max {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid elastic-team limit", v.Description(ctx))
	}
}

type elasticNonemptyValidator struct{}

func (elasticNonemptyValidator) Description(context.Context) string {
	return "must contain at least one entry"
}
func (v elasticNonemptyValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v elasticNonemptyValidator) ValidateList(ctx context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if len(req.ConfigValue.Elements()) == 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Empty elastic-team selection", v.Description(ctx))
	}
}
func (v elasticNonemptyValidator) ValidateMap(ctx context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if len(req.ConfigValue.Elements()) == 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Empty elastic-team templates", v.Description(ctx))
	}
}
