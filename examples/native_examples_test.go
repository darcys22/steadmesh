package examples_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// TestNativeExamplesCompile evaluates the checked-in HCL with the same pinned
// role reference produced by instruction-bundle. This offline check exercises
// concrete spec validation without substituting for terraform validate or apply.
func TestNativeExamplesCompile(t *testing.T) {
	for _, dir := range []string{"native-hybrid", "native-self-organising", "native-fixed-prompts"} {
		t.Run(dir, func(t *testing.T) {
			parse := func(name string) *hclsyntax.Body {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				f, d := hclsyntax.ParseConfig(data, name, hcl.InitialPos)
				if d.HasErrors() {
					t.Fatal(d.Error())
				}
				return f.Body.(*hclsyntax.Body)
			}
			vars := map[string]cty.Value{}
			for _, b := range parse("variables.tf").Blocks {
				if b.Type == "variable" {
					v, d := b.Body.Attributes["default"].Expr.Value(nil)
					if d.HasErrors() {
						t.Fatal(d.Error())
					}
					vars[b.Labels[0]] = v
				}
			}
			role, err := os.ReadFile(filepath.Join(dir, "role.md"))
			if err != nil {
				t.Fatal(err)
			}
			ref := fmt.Sprintf("configmap:%s-role/role.md#sha256:%x", dir, sha256.Sum256(role))
			ctx := &hcl.EvalContext{Variables: map[string]cty.Value{
				"var":    cty.ObjectVal(vars),
				"module": cty.ObjectVal(map[string]cty.Value{"role": cty.ObjectVal(map[string]cty.Value{"ref": cty.StringVal(ref)})}),
			}, Functions: map[string]function.Function{"merge": stdlib.MergeFunc}}
			body := parse("main.tf")
			for _, b := range body.Blocks {
				if b.Type == "locals" {
					values := map[string]cty.Value{}
					for k, a := range b.Body.Attributes {
						v, d := a.Expr.Value(ctx)
						if d.HasErrors() {
							t.Fatal(d.Error())
						}
						values[k] = v
					}
					ctx.Variables["local"] = cty.ObjectVal(values)
				}
			}
			found := false
			for _, b := range body.Blocks {
				if b.Type != "resource" || b.Labels[0] != "steadmesh_organization" {
					continue
				}
				found = true
				v, d := b.Body.Attributes["spec"].Expr.Value(ctx)
				if d.HasErrors() {
					t.Fatal(d.Error())
				}
				raw, err := ctyjson.Marshal(v, v.Type())
				if err != nil {
					t.Fatal(err)
				}
				var s spec.OrganizationSpec
				if err = json.Unmarshal(raw, &s); err != nil {
					t.Fatal(err)
				}
				for field, dst := range map[string]*string{"key": &s.Key, "display_name": &s.DisplayName, "data_retention": &s.DataRetention} {
					v, d := b.Body.Attributes[field].Expr.Value(ctx)
					if d.HasErrors() {
						t.Fatal(d.Error())
					}
					*dst = v.AsString()
				}
				m, err := compile.Compile(s, compile.DefaultCatalog())
				if err != nil {
					t.Fatal(err)
				}
				if len(m.Seats) != len(s.Seats) {
					t.Fatal("compile materialized runtime pool members into declared seats")
				}
				pool := s.Teams["delivery"].Pool
				if pool.RetentionSeconds != 0 {
					t.Fatal("examples must preserve retired private memory by default")
				}
				if dir == "native-hybrid" {
					c := s.Teams["delivery"].Context
					if !c.RequireInstructionApproval || !c.EditableInstructions || len(c.InstructionApprovers) != 3 {
						t.Fatal("hybrid instructions must require explicit leader approval")
					}
				}
				if pool.MinMembers != 3 {
					t.Fatal("example must keep three available pool members")
				}
				if dir == "native-hybrid" && pool.MaxMembers != 30 {
					t.Fatal("hybrid pool must scale to thirty")
				}
				if dir == "native-fixed-prompts" {
					c := s.Teams["delivery"].Context
					if c.EditableInstructions || !c.EditableSummary {
						t.Fatal("fixed prompts must preserve independent editable summary")
					}
				}
			}
			if !found {
				t.Fatal("organisation resource not found")
			}
		})
	}
}
