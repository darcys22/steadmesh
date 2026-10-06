package provider

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"sigs.k8s.io/yaml"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

const fixturePath = "../tests/fixtures/valid/representative_and_worker.yaml"

func loadFixture(t *testing.T) spec.OrganizationSpec {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var s spec.OrganizationSpec
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func specType(ctx context.Context) attr.Type {
	return organizationSchema(ctx).Attributes["spec"].GetType()
}

// fixtureModel returns a planned model built from the fixture.
func fixtureModel(t *testing.T) organizationModel {
	t.Helper()
	ctx := context.Background()
	d, err := declarationFromSpec(loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	sv, err := treeToAttr(ctx, specType(ctx), d.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return organizationModel{
		ID:                types.StringUnknown(),
		Key:               types.StringValue(d.Key.(string)),
		DisplayName:       types.StringValue(d.DisplayName.(string)),
		Spec:              sv.(types.Object),
		DataRetention:     types.StringNull(),
		WaitForReady:      types.BoolValue(false),
		Timeouts:          nullTimeouts(t),
		ManifestDigest:    types.StringUnknown(),
		ResolvedSeats:     types.MapUnknown(types.ObjectType{AttrTypes: resolvedSeatAttrTypes}),
		Conditions:        types.ListUnknown(types.ObjectType{AttrTypes: conditionAttrTypes}),
		EffectiveRevision: types.StringUnknown(),
		ConnectionDetails: types.ObjectUnknown(connectionDetailsAttrTypes),
	}
}

// TestSpecRoundTrip converts the fixture spec into the Terraform model and
// back, and checks that nothing is lost or invented.
func TestSpecRoundTrip(t *testing.T) {
	ctx := context.Background()
	orig := loadFixture(t)
	m := fixtureModel(t)
	d, err := declarationFromModel(ctx, &m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.toOrganizationSpec()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonicalSpecJSON(orig), canonicalSpecJSON(got)) {
		t.Fatalf("round trip changed the spec:\n%s\n%s", canonicalSpecJSON(orig), canonicalSpecJSON(got))
	}
	want, err := compile.Compile(orig, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	cm, diags := compileDeclaration(ctx, d)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if cm.Digest != want.Digest {
		t.Fatalf("digest %s != %s", cm.Digest, want.Digest)
	}
}

// TestDeclarationAnnotationRoundTrip checks that the stored declaration
// reproduces the exact Terraform value, including explicit nulls, empty
// collections and false booleans, which an import relies on.
func TestDeclarationAnnotationRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := fixtureModel(t)
	d, err := declarationFromModel(ctx, &m)
	if err != nil {
		t.Fatal(err)
	}
	sm := d.Spec.(map[string]any)
	sm["message_routes"].(map[string]any)["rep_to_reviewer"].(map[string]any)["bidirectional"] = false
	sm["memory_stores"].(map[string]any)["organisation"].(map[string]any)["capacity_mb"] = int64(128)
	sm["culture_refs"] = []any{}
	before, err := treeToAttr(ctx, specType(ctx), d.Spec)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encodeDeclaration(d)
	if err != nil {
		t.Fatal(err)
	}
	back, ok := decodeDeclaration(enc)
	if !ok {
		t.Fatal("decode failed")
	}
	after, err := treeToAttr(ctx, specType(ctx), back.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatalf("declaration changed:\n%s\n%s", before, after)
	}
	if back.Key != d.Key || back.DisplayName != d.DisplayName || back.DataRetention != nil {
		t.Fatalf("top-level fields changed: %+v", back)
	}
}

// TestSchemaMirrorsSpec checks that the `spec` attribute mirrors
// spec.OrganizationSpec field-for-field, in both directions.
func TestSchemaMirrorsSpec(t *testing.T) {
	ctx := context.Background()
	st := specType(ctx).(types.ObjectType)
	var problems []string
	var walk func(p string, rt reflect.Type, at attr.Type, skip map[string]bool)
	walk = func(p string, rt reflect.Type, at attr.Type, skip map[string]bool) {
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		switch rt.Kind() {
		case reflect.Struct:
			ot, ok := at.(types.ObjectType)
			if !ok {
				problems = append(problems, p+": expected object")
				return
			}
			seen := map[string]bool{}
			for i := 0; i < rt.NumField(); i++ {
				name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
				if skip[name] {
					continue
				}
				seen[name] = true
				sub, ok := ot.AttrTypes[name]
				if !ok {
					problems = append(problems, p+"."+name+": missing from schema")
					continue
				}
				walk(p+"."+name, rt.Field(i).Type, sub, nil)
			}
			for name := range ot.AttrTypes {
				if !seen[name] {
					problems = append(problems, p+"."+name+": not in pkg/spec")
				}
			}
		case reflect.Map:
			mt, ok := at.(types.MapType)
			if !ok {
				problems = append(problems, p+": expected map")
				return
			}
			walk(p+"[*]", rt.Elem(), mt.ElemType, nil)
		case reflect.Slice:
			lt, ok := at.(types.ListType)
			if !ok {
				problems = append(problems, p+": expected list")
				return
			}
			walk(p+"[]", rt.Elem(), lt.ElemType, nil)
		case reflect.String:
			if !at.Equal(types.StringType) {
				problems = append(problems, p+": expected string")
			}
		case reflect.Bool:
			if !at.Equal(types.BoolType) {
				problems = append(problems, p+": expected bool")
			}
		case reflect.Int64, reflect.Int:
			if !at.Equal(types.Int64Type) {
				problems = append(problems, p+": expected int64")
			}
		default:
			problems = append(problems, p+": unsupported kind "+rt.Kind().String())
		}
	}
	skip := map[string]bool{}
	for _, f := range topLevelFields {
		skip[f] = true
	}
	walk("spec", reflect.TypeOf(spec.OrganizationSpec{}), st, skip)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("schema and pkg/spec differ:\n%s", strings.Join(problems, "\n"))
	}
	for _, f := range topLevelFields {
		if _, ok := organizationSchema(ctx).Attributes[f]; !ok {
			t.Fatalf("top-level attribute %s missing", f)
		}
	}
}

func TestFieldErrorPath(t *testing.T) {
	ctx := context.Background()
	st := specType(ctx)
	spec := path.Root("spec")
	cases := map[string]path.Path{
		"key":                                      path.Root("key"),
		"data_retention":                           path.Root("data_retention"),
		"spec":                                     path.Root("spec"),
		"culture_refs[0]":                          spec.AtName("culture_refs").AtListIndex(0),
		"seats.reviewer.role_ref":                  spec.AtName("seats").AtMapKey("reviewer").AtName("role_ref"),
		"seats.reviewer.workspace.shared":          spec.AtName("seats").AtMapKey("reviewer").AtName("workspace").AtName("shared"),
		"teams.eng.instruction_refs[1]":            spec.AtName("teams").AtMapKey("eng").AtName("instruction_refs").AtListIndex(1),
		"teams.eng.shared_memory.engineering":      spec.AtName("teams").AtMapKey("eng").AtName("shared_memory").AtMapKey("engineering"),
		"execution_profiles.interactive.bogus.x":   spec.AtName("execution_profiles").AtMapKey("interactive"),
		"grants.reviewer_tracker.operations":       spec.AtName("grants").AtMapKey("reviewer_tracker").AtName("operations"),
		"channel_bindings.sean.external_user_id":   spec.AtName("channel_bindings").AtMapKey("sean").AtName("external_user_id"),
		"harness_profiles.fake.model_connection":   spec.AtName("harness_profiles").AtMapKey("fake").AtName("model_connection"),
		"team_templates.base":                      spec.AtName("team_templates").AtMapKey("base"),
		"message_routes.rep_to_reviewer.from":      spec.AtName("message_routes").AtMapKey("rep_to_reviewer").AtName("from"),
		"sandbox_profiles.standard.runtime_class":  spec.AtName("sandbox_profiles").AtMapKey("standard").AtName("runtime_class"),
		"connections.tracker.secret_ref":           spec.AtName("connections").AtMapKey("tracker").AtName("secret_ref"),
		"memory_stores.organisation.backing_class": spec.AtName("memory_stores").AtMapKey("organisation").AtName("backing_class"),
	}
	for in, want := range cases {
		if got := fieldErrorPath(st, in); !got.Equal(want) {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestCompileErrorsMapToAttributes(t *testing.T) {
	ctx := context.Background()
	m := fixtureModel(t)
	d, _ := declarationFromModel(ctx, &m)
	seats := d.Spec.(map[string]any)["seats"].(map[string]any)
	seats["reviewer"].(map[string]any)["harness_profile"] = "missing"
	_, diags := compileDeclaration(ctx, d)
	if !diags.HasError() {
		t.Fatal("expected errors")
	}
	want := path.Root("spec").AtName("seats").AtMapKey("reviewer").AtName("harness_profile")
	for _, dg := range diags.Errors() {
		if wp, ok := dg.(interface{ Path() path.Path }); ok && wp.Path().Equal(want) && strings.Contains(dg.Detail(), "unknown harness profile") {
			return
		}
	}
	t.Fatalf("no diagnostic at %s: %v", want, diags)
}
