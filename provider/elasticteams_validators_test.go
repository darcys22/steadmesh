package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestElasticLimitValidators(t *testing.T) {
	ctx := context.Background()
	policy := specAttributes()["elastic_teams"].(schema.SingleNestedAttribute)
	for field, max := range map[string]int64{"max_active_workers": 1024, "max_active_per_execution": 32, "max_launches_per_execution": 128, "max_timeout_seconds": 1200} {
		a := policy.Attributes[field].(schema.Int64Attribute)
		if len(a.Validators) != 1 {
			t.Fatalf("%s: missing field validator", field)
		}
		for _, tc := range []struct {
			name    string
			value   types.Int64
			invalid bool
		}{
			{"null", types.Int64Null(), false}, {"unknown", types.Int64Unknown(), false},
			{"negative", types.Int64Value(-1), true}, {"zero", types.Int64Value(0), true},
			{"minimum", types.Int64Value(1), false}, {"maximum", types.Int64Value(max), false}, {"over", types.Int64Value(max + 1), true},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				p := path.Root("spec").AtName("elastic_teams").AtName(field)
				var resp validator.Int64Response
				a.Validators[0].ValidateInt64(ctx, validator.Int64Request{Path: p, ConfigValue: tc.value}, &resp)
				if resp.Diagnostics.HasError() != tc.invalid {
					t.Fatalf("diagnostics=%v", resp.Diagnostics)
				}
			})
		}
	}
}

func TestElasticNonemptyValidators(t *testing.T) {
	ctx := context.Background()
	policy := specAttributes()["elastic_teams"].(schema.SingleNestedAttribute)
	seats := policy.Attributes["enabled_seats"].(schema.ListAttribute)
	templates := policy.Attributes["templates"].(schema.MapNestedAttribute)
	if len(seats.Validators) != 1 || len(templates.Validators) != 1 {
		t.Fatal("missing collection validators")
	}
	for _, tc := range []struct {
		name    string
		value   types.List
		invalid bool
	}{
		{"null", types.ListNull(types.StringType), false}, {"unknown", types.ListUnknown(types.StringType), false},
		{"empty", types.ListValueMust(types.StringType, []attr.Value{}), true},
		{"known", types.ListValueMust(types.StringType, []attr.Value{types.StringValue("engineer")}), false},
		{"unknown_element", types.ListValueMust(types.StringType, []attr.Value{types.StringUnknown()}), false},
	} {
		t.Run("seats/"+tc.name, func(t *testing.T) {
			var resp validator.ListResponse
			seats.Validators[0].ValidateList(ctx, validator.ListRequest{Path: path.Root("enabled_seats"), ConfigValue: tc.value}, &resp)
			if resp.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("diagnostics=%v", resp.Diagnostics)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		value   types.Map
		invalid bool
	}{
		{"null", types.MapNull(types.StringType), false}, {"unknown", types.MapUnknown(types.StringType), false},
		{"empty", types.MapValueMust(types.StringType, map[string]attr.Value{}), true},
		{"known", types.MapValueMust(types.StringType, map[string]attr.Value{"researcher": types.StringValue("instructions")}), false},
		{"unknown_element", types.MapValueMust(types.StringType, map[string]attr.Value{"researcher": types.StringUnknown()}), false},
	} {
		t.Run("templates/"+tc.name, func(t *testing.T) {
			var resp validator.MapResponse
			templates.Validators[0].ValidateMap(ctx, validator.MapRequest{Path: path.Root("templates"), ConfigValue: tc.value}, &resp)
			if resp.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("diagnostics=%v", resp.Diagnostics)
			}
		})
	}
}
