package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSkillsRoundTrip(t *testing.T) {
	for _, mode := range []string{"", "explicit", "discover", "agent_choice"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := loadFixture(t)
			s.SkillMarketplaces = map[string]spec.SkillMarketplace{"approved": {Repository: "example/skills", Ref: strings.Repeat("a", 40)}}
			seat := s.Seats["reviewer"]
			seat.Skills = &spec.SkillPolicy{Mode: mode, Marketplaces: []string{"approved"}, Install: []spec.SkillRef{{Marketplace: "approved", Plugin: "documents", Skill: "pdf"}}}
			if mode == "agent_choice" {
				seat.Skills.MaxDynamicSkills = 16
			}
			s.Seats["reviewer"] = seat
			d, err := declarationFromSpec(s)
			if err != nil {
				t.Fatal(err)
			}
			v, err := treeToAttr(ctx, specType(ctx), d.Spec)
			if err != nil {
				t.Fatal(err)
			}
			d.Spec, err = attrToTree(ctx, v)
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.toOrganizationSpec()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s, got) {
				t.Fatalf("roundtrip changed skills: %+v", got)
			}
			encoded, err := encodeDeclaration(d)
			if err != nil {
				t.Fatal(err)
			}
			back, ok := decodeDeclaration(encoded)
			if !ok {
				t.Fatal("declaration decode failed")
			}
			after, err := treeToAttr(ctx, specType(ctx), back.Spec)
			if err != nil || !v.Equal(after) {
				t.Fatalf("declaration annotation changed: %v", err)
			}
		})
	}
}

func TestSkillDynamicLimitValidator(t *testing.T) {
	a := skillPolicyAttribute().Attributes["max_dynamic_skills"].(schema.Int64Attribute)
	for _, tc := range []struct {
		value   types.Int64
		invalid bool
	}{
		{types.Int64Null(), false}, {types.Int64Unknown(), false},
		{types.Int64Value(-1), true}, {types.Int64Value(0), true},
		{types.Int64Value(1), false}, {types.Int64Value(128), false}, {types.Int64Value(129), true},
	} {
		var resp validator.Int64Response
		a.Validators[0].ValidateInt64(context.Background(), validator.Int64Request{Path: path.Root("max_dynamic_skills"), ConfigValue: tc.value}, &resp)
		if resp.Diagnostics.HasError() != tc.invalid {
			t.Fatalf("%v: %v", tc.value, resp.Diagnostics)
		}
	}
}
