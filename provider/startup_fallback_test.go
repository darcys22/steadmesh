package provider

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"sigs.k8s.io/yaml"
)

func TestStartupFallbackRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := loadFixture(t)
	h := s.HarnessProfiles["fake"]
	h.StartupFallback = &spec.StartupFallbackPolicy{Profiles: []string{"secondary", "third"}, MaxAttempts: 3, TotalTimeout: "45s"}
	s.HarnessProfiles["fake"] = h
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
	if err != nil || !reflect.DeepEqual(s, got) {
		t.Fatalf("fallback roundtrip changed spec: %v\n%+v", err, got)
	}
}

func TestStartupFallbackFieldValidators(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		value   types.Int64
		invalid bool
	}{
		{types.Int64Null(), false}, {types.Int64Unknown(), false},
		{types.Int64Value(0), true}, {types.Int64Value(-1), true}, {types.Int64Value(1), false}, {types.Int64Value(8), false}, {types.Int64Value(9), true},
	} {
		var resp validator.Int64Response
		startupFallbackAttemptsValidator{}.ValidateInt64(ctx, validator.Int64Request{Path: path.Root("max_attempts"), ConfigValue: tc.value}, &resp)
		if resp.Diagnostics.HasError() != tc.invalid {
			t.Fatalf("attempts %v: %v", tc.value, resp.Diagnostics)
		}
	}
	for _, tc := range []struct {
		value   types.String
		invalid bool
	}{
		{types.StringNull(), false}, {types.StringUnknown(), false},
		{types.StringValue(""), true}, {types.StringValue("bad"), true}, {types.StringValue("0s"), true}, {types.StringValue("-1s"), true},
		{types.StringValue("1ns"), false}, {types.StringValue("5m"), false}, {types.StringValue("5m1ns"), true},
	} {
		var resp validator.StringResponse
		startupFallbackTimeoutValidator{}.ValidateString(ctx, validator.StringRequest{Path: path.Root("total_timeout"), ConfigValue: tc.value}, &resp)
		if resp.Diagnostics.HasError() != tc.invalid {
			t.Fatalf("timeout %v: %v", tc.value, resp.Diagnostics)
		}
	}
	for _, n := range []int{0, 1, 7, 8} {
		values := make([]attr.Value, n)
		for i := range values {
			values[i] = types.StringValue("candidate")
		}
		value := types.ListValueMust(types.StringType, values)
		var resp validator.ListResponse
		startupFallbackProfilesValidator{}.ValidateList(ctx, validator.ListRequest{Path: path.Root("profiles"), ConfigValue: value}, &resp)
		if resp.Diagnostics.HasError() != (n < 1 || n > 7) {
			t.Fatalf("profiles count %d: %v", n, resp.Diagnostics)
		}
	}
}

func TestStartupFallbackCRDSchema(t *testing.T) {
	data, err := os.ReadFile("../charts/platform/crds/steadmesh.io_agentorganizations.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	// Check the generated schema retains the block rather than pruning it at
	// admission, and enforces the schema-level count/attempt bounds.
	text := string(data)
	for _, expected := range []string{"startup_fallback:", "max_attempts:", "maximum: 8", "profiles:", "maxItems: 7", "minItems: 1", "total_timeout:"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("CRD omits %q", expected)
		}
	}
}
