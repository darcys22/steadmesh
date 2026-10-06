package compile_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

const fixtures = "../../tests/fixtures"

func load(t *testing.T, path string) (spec.OrganizationSpec, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var expect string
	if first, _, ok := strings.Cut(string(b), "\n"); ok {
		expect, _ = strings.CutPrefix(first, "# expect: ")
		if expect == first {
			expect = ""
		}
	}
	var s spec.OrganizationSpec
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return s, expect
}

func compileValid(t *testing.T) *compile.Manifest {
	t.Helper()
	s, _ := load(t, filepath.Join(fixtures, "valid", "representative_and_worker.yaml"))
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	return m
}

func TestRejectedFixtures(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(fixtures, "invalid", "*.yaml"))
	if len(files) == 0 {
		t.Fatal("no invalid fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			s, expect := load(t, f)
			if expect == "" {
				t.Fatal("fixture lacks '# expect:' header")
			}
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), expect) {
				t.Fatalf("error %q does not contain %q", err, expect)
			}
		})
	}
}

func TestTemplateResolution(t *testing.T) {
	m := compileValid(t)
	if m.Spec.TeamTemplates != nil {
		t.Fatal("templates must be removed from the resolved spec")
	}
	eng := m.Spec.Teams["engineering"]
	if len(eng.InstructionRefs) != 2 || !strings.Contains(eng.InstructionRefs[0], "team-base") {
		t.Fatalf("instruction refs not base-first: %v", eng.InstructionRefs)
	}
	if !slices.Equal(m.TemplateLineage["engineering"], []string{"base", "engineering"}) {
		t.Fatalf("lineage = %v", m.TemplateLineage["engineering"])
	}
	rev := m.Seats["reviewer"]
	if !strings.Contains(rev.RoleRef, "roles/reviewer.md") {
		t.Fatalf("role:reviewer not resolved: %s", rev.RoleRef)
	}
	var scopes []string
	for _, i := range rev.Instructions {
		scopes = append(scopes, i.Scope)
	}
	want := []string{"organisation", "team:engineering", "team:engineering", "role"}
	if !slices.Equal(scopes, want) {
		t.Fatalf("instruction order = %v, want %v", scopes, want)
	}
}

func TestCapabilities(t *testing.T) {
	m := compileValid(t)
	caps := func(seat string) map[string]compile.Capability {
		out := map[string]compile.Capability{}
		for _, c := range m.Seats[seat].Capabilities {
			out[c.Resource] = c
		}
		return out
	}
	rev := caps("reviewer")
	if c, ok := rev["memory:reviewer"]; !ok || !slices.Contains(c.Sources, "implicit:personal_memory") {
		t.Fatalf("personal memory not implicitly granted: %+v", rev)
	}
	if _, ok := rev["memory:rep_sean"]; ok {
		t.Fatal("reviewer must not see the representative's private memory")
	}
	if c := rev["memory:engineering"]; !slices.Equal(c.Operations, []string{"read", "revise", "search", "write"}) {
		t.Fatalf("team shared memory ops = %v", c.Operations)
	}
	if c := rev["connection:tracker"]; !slices.Contains(c.Operations, "project.create") {
		t.Fatalf("tracker grant missing: %+v", c)
	}
	rep := caps("representative_sean")
	if c := rep["connection:slack"]; !slices.Equal(c.Operations, []string{"channel.reply"}) || !slices.Equal(c.Targets, []string{"U0SEAN"}) {
		t.Fatalf("binding reply capability = %+v", c)
	}
	if _, ok := rep["memory:engineering"]; ok {
		t.Fatal("representative is not an engineering member")
	}
	if !m.Seats["representative_sean"].IsRepresentative || m.Seats["reviewer"].IsRepresentative {
		t.Fatal("representative flag wrong")
	}
	sendTo := m.Seats["representative_sean"].SendTo
	if len(sendTo) != 1 || sendTo[0].Seat != "reviewer" || !sendTo[0].Reply {
		t.Fatalf("route = %+v", sendTo)
	}
	if len(m.Seats["reviewer"].SendTo) != 0 {
		t.Fatal("reply semantics must not create a general reviewer->rep route")
	}
}

func TestDefaultsVisibleAndDeterministic(t *testing.T) {
	a, b := compileValid(t), compileValid(t)
	if a.Digest != b.Digest {
		t.Fatal("compilation is not deterministic")
	}
	if a.DefaultsVersion != spec.DefaultsVersion {
		t.Fatal("defaults version missing")
	}
	e := a.Spec.ExecutionProfiles["interactive"]
	if e.IdlePolicy != "warm_then_stop" || e.MemoryLimit == "" || e.IdleTimeout == "" {
		t.Fatalf("defaults not materialised: %+v", e)
	}
	if !slices.Contains(a.Spec.SandboxProfiles["standard"].RequiredEnforcement, "network_policy") {
		t.Fatal("network policy enforcement must be required by default")
	}
	// Recompiling the resolved spec is a fixed point: the controller recompiles
	// what the provider wrote and must get the same seats.
	again, err := compile.Compile(a.Spec, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for k := range a.Seats {
		if a.Seats[k].ConfigRevision != again.Seats[k].ConfigRevision {
			t.Fatalf("seat %s revision changed on recompilation", k)
		}
	}
}

func TestInstructionChangeChangesOnlyAffectedSeats(t *testing.T) {
	s, _ := load(t, filepath.Join(fixtures, "valid", "representative_and_worker.yaml"))
	before, _ := compile.Compile(s, compile.DefaultCatalog())
	tt := s.TeamTemplates["engineering"]
	tt.InstructionRefs = []string{"configmap:team-eng/engineering.md#sha256:" + strings.Repeat("b", 64)}
	s.TeamTemplates["engineering"] = tt
	after, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if before.Seats["reviewer"].ConfigRevision == after.Seats["reviewer"].ConfigRevision {
		t.Fatal("reviewer revision should change")
	}
	if before.Seats["representative_sean"].ConfigRevision != after.Seats["representative_sean"].ConfigRevision {
		t.Fatal("representative revision should not change")
	}
}
