package compile_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func skillSpec(t *testing.T) spec.OrganizationSpec {
	t.Helper()
	s := mixed(t, spec.HarnessProfile{Adapter: "claude-code", Model: &spec.ModelSelection{Connection: "anthropic", ID: "claude-test"}})
	s.SkillMarketplaces = map[string]spec.SkillMarketplace{
		"approved": {Repository: "example/skills", Ref: strings.Repeat("a", 40)},
		"other":    {Repository: "example/other", Ref: strings.Repeat("b", 40)},
	}
	seat := s.Seats["reviewer"]
	seat.Skills = &spec.SkillPolicy{
		Marketplaces: []string{"approved"},
		Install:      []spec.SkillRef{{Marketplace: "approved", Plugin: "documents", Skill: "pdf"}},
	}
	s.Seats["reviewer"] = seat
	return s
}

func TestSkillPolicyCompile(t *testing.T) {
	for _, mode := range []string{"", "explicit", "discover", "agent_choice"} {
		t.Run("mode="+mode, func(t *testing.T) {
			s := skillSpec(t)
			s.Seats["reviewer"].Skills.Mode = mode
			m, err := compile.Compile(s, compile.DefaultCatalog())
			if err != nil {
				t.Fatal(err)
			}
			got := m.Seats["reviewer"]
			wantMode := mode
			if wantMode == "" {
				wantMode = "explicit"
			}
			if got.Skills == nil || got.Skills.Mode != wantMode {
				t.Fatalf("policy=%+v", got.Skills)
			}
			if wantMode == "agent_choice" && got.Skills.MaxDynamicSkills != spec.DefaultMaxDynamicSkills {
				t.Fatalf("cap=%d", got.Skills.MaxDynamicSkills)
			}
			if len(got.SkillMarketplaces) != 1 || got.SkillMarketplaces["approved"] != s.SkillMarketplaces["approved"] {
				t.Fatalf("sources=%+v", got.SkillMarketplaces)
			}
			if got.Skills.Install[0].Plugin != "documents" || got.Skills.Install[0].Skill != "pdf" {
				t.Fatalf("plugin and skill conflated: %+v", got.Skills.Install)
			}
			if m.Seats["representative_sean"].Skills != nil || len(m.Seats["representative_sean"].SkillMarketplaces) != 0 {
				t.Fatal("skills leaked to an unconfigured seat")
			}
			if s.Seats["reviewer"].Skills.Mode != mode || s.Seats["reviewer"].Skills.MaxDynamicSkills != 0 {
				t.Fatal("compile mutated input")
			}
			got.Skills.Install[0].Skill = "changed"
			if m.Spec.Seats["reviewer"].Skills.Install[0].Skill != "pdf" {
				t.Fatal("seat manifest aliases resolved spec")
			}
		})
	}
}

func TestSkillHarnessSupport(t *testing.T) {
	for _, adapter := range []string{"claude-code", "codex", "pi"} {
		s := skillSpec(t)
		h := s.HarnessProfiles["h"]
		h.Adapter = adapter
		if adapter == "codex" {
			h.Model = &spec.ModelSelection{Connection: "openai", ID: "model"}
		}
		s.HarnessProfiles["h"] = h
		if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
			t.Fatalf("%s: %v", adapter, err)
		}
	}
}

func TestSkillValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*spec.OrganizationSpec)
		path   string
	}{
		{"url", func(s *spec.OrganizationSpec) {
			s.SkillMarketplaces["approved"] = spec.SkillMarketplace{Repository: "https://github.com/example/skills", Ref: strings.Repeat("a", 40)}
		}, "skill_marketplaces.approved.repository"},
		{"path", func(s *spec.OrganizationSpec) {
			s.SkillMarketplaces["approved"] = spec.SkillMarketplace{Repository: "owner/..", Ref: strings.Repeat("a", 40)}
		}, "skill_marketplaces.approved.repository"},
		{"ref omitted", func(s *spec.OrganizationSpec) {
			s.SkillMarketplaces["approved"] = spec.SkillMarketplace{Repository: "example/skills"}
		}, "skill_marketplaces.approved.ref"},
		{"branch", func(s *spec.OrganizationSpec) {
			s.SkillMarketplaces["approved"] = spec.SkillMarketplace{Repository: "example/skills", Ref: "main"}
		}, "skill_marketplaces.approved.ref"},
		{"tag", func(s *spec.OrganizationSpec) {
			s.SkillMarketplaces["approved"] = spec.SkillMarketplace{Repository: "example/skills", Ref: "v1.0"}
		}, "skill_marketplaces.approved.ref"},
		{"unsafe key", func(s *spec.OrganizationSpec) { s.SkillMarketplaces["../bad"] = s.SkillMarketplaces["approved"] }, "skill_marketplaces.../bad"},
		{"unknown marketplace", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Marketplaces = []string{"missing"} }, "skills.marketplaces"},
		{"empty allowlist", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Marketplaces = nil }, "skills.marketplaces"},
		{"duplicate marketplace", func(s *spec.OrganizationSpec) {
			s.Seats["reviewer"].Skills.Marketplaces = []string{"approved", "approved"}
		}, "skills.marketplaces"},
		{"unallowed install", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Install[0].Marketplace = "other" }, "skills.install[0].marketplace"},
		{"plugin traversal", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Install[0].Plugin = "../x" }, "skills.install[0].plugin"},
		{"skill traversal", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Install[0].Skill = "../../secret" }, "skills.install[0].skill"},
		{"missing skill", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Install[0].Skill = "" }, "skills.install[0].skill"},
		{"duplicate ref", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			p.Install = append(p.Install, p.Install[0])
		}, "skills.install[1]"},
		{"invalid mode", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Mode = "anything" }, "skills.mode"},
		{"negative cap", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			p.Mode = "agent_choice"
			p.MaxDynamicSkills = -1
		}, "skills.max_dynamic_skills"},
		{"huge cap", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			p.Mode = "agent_choice"
			p.MaxDynamicSkills = 129
		}, "skills.max_dynamic_skills"},
		{"explicit cap", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.MaxDynamicSkills = 1 }, "skills.max_dynamic_skills"},
		{"discover cap", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			p.Mode = "discover"
			p.MaxDynamicSkills = 1
		}, "skills.max_dynamic_skills"},
		{"unsupported harness", func(s *spec.OrganizationSpec) {
			h := s.HarnessProfiles["h"]
			h.Adapter = "fake"
			s.HarnessProfiles["h"] = h
		}, "seats.reviewer.skills"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := skillSpec(t)
			tc.mutate(&s)
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error=%v, want path %s", err, tc.path)
			}
		})
	}
}

func TestSkillRevision(t *testing.T) {
	s := skillSpec(t)
	base, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		mutate      func(*spec.OrganizationSpec)
		affectsSeat bool
	}{
		{"pin", func(s *spec.OrganizationSpec) {
			source := s.SkillMarketplaces["approved"]
			source.Ref = strings.Repeat("c", 40)
			s.SkillMarketplaces["approved"] = source
		}, true},
		{"repository", func(s *spec.OrganizationSpec) {
			source := s.SkillMarketplaces["approved"]
			source.Repository = "example/updated"
			s.SkillMarketplaces["approved"] = source
		}, true},
		{"mode", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Mode = "discover" }, true},
		{"allowlist", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			p.Marketplaces = append(p.Marketplaces, "other")
		}, true},
		{"install", func(s *spec.OrganizationSpec) { s.Seats["reviewer"].Skills.Install[0].Skill = "docx" }, true},
		{"unallowed source", func(s *spec.OrganizationSpec) {
			source := s.SkillMarketplaces["other"]
			source.Ref = strings.Repeat("c", 40)
			s.SkillMarketplaces["other"] = source
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := s.DeepCopy()
			tc.mutate(modified)
			got, err := compile.Compile(*modified, compile.DefaultCatalog())
			if err != nil {
				t.Fatal(err)
			}
			changed := got.Seats["reviewer"].ConfigRevision != base.Seats["reviewer"].ConfigRevision
			if changed != tc.affectsSeat {
				t.Fatalf("seat revision changed=%v", changed)
			}
			if got.Digest == base.Digest {
				t.Fatal("organization digest unchanged")
			}
			if got.Seats["representative_sean"].ConfigRevision != base.Seats["representative_sean"].ConfigRevision {
				t.Fatal("unconfigured seat revision changed")
			}
		})
	}
	copy := s.DeepCopy()
	copy.Seats["reviewer"].Skills.Marketplaces[0] = "changed"
	copy.Seats["reviewer"].Skills.Install[0].Skill = "changed"
	copy.SkillMarketplaces["approved"] = spec.SkillMarketplace{}
	if !reflect.DeepEqual(s, skillSpec(t)) {
		t.Fatal("deep copy aliased original")
	}
}

func TestSkillDynamicCapRevision(t *testing.T) {
	s := skillSpec(t)
	s.Seats["reviewer"].Skills.Mode = "agent_choice"
	before, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	s.Seats["reviewer"].Skills.MaxDynamicSkills = 1
	after, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if before.Seats["reviewer"].ConfigRevision == after.Seats["reviewer"].ConfigRevision {
		t.Fatal("dynamic skill cap omitted from configuration revision")
	}
}

func TestPoolSeatSkills(t *testing.T) {
	s := nativePoolSpec(t)
	configured := skillSpec(t)
	s.HarnessProfiles["h"] = configured.HarnessProfiles["h"]
	s.Connections["anthropic"] = configured.Connections["anthropic"]
	s.SkillMarketplaces = configured.SkillMarketplaces
	seat := s.Teams["pool_team"].Pool.Templates["builder"].Seat
	seat.HarnessProfile = "h"
	seat.Skills = configured.Seats["reviewer"].Skills.DeepCopy()
	seat.Skills.Mode = "agent_choice"
	sm, err := compile.CompilePoolSeat(s, compile.DefaultCatalog(), "pool_team", "builder", "member_one")
	if err != nil {
		t.Fatal(err)
	}
	if sm.Skills == nil || sm.Skills.MaxDynamicSkills != 16 || len(sm.SkillMarketplaces) != 1 {
		t.Fatalf("pool skills=%+v sources=%+v", sm.Skills, sm.SkillMarketplaces)
	}
	seat.Skills.Install[0].Marketplace = "other"
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err == nil || !strings.Contains(err.Error(), "pool.templates.builder.seat") {
		t.Fatalf("invalid pool skill policy escaped validation: %v", err)
	}
}

func TestSkillCollectionLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*spec.OrganizationSpec)
		path   string
	}{
		{"sources", func(s *spec.OrganizationSpec) {
			for i := 0; i < spec.MaxOrganizationSkillMarketplaces; i++ {
				s.SkillMarketplaces[fmt.Sprintf("source_%d", i)] = s.SkillMarketplaces["approved"]
			}
		}, "skill_marketplaces"},
		{"allowlist", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			for i := 0; i < spec.MaxSeatSkillMarketplaces; i++ {
				k := fmt.Sprintf("source_%d", i)
				s.SkillMarketplaces[k] = s.SkillMarketplaces["approved"]
				p.Marketplaces = append(p.Marketplaces, k)
			}
		}, "seats.reviewer.skills.marketplaces"},
		{"install", func(s *spec.OrganizationSpec) {
			p := s.Seats["reviewer"].Skills
			for i := 0; i < spec.MaxStaticSkills; i++ {
				p.Install = append(p.Install, spec.SkillRef{Marketplace: "approved", Plugin: "documents", Skill: fmt.Sprintf("skill-%d", i)})
			}
		}, "seats.reviewer.skills.install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := skillSpec(t)
			tc.mutate(&s)
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || !strings.Contains(err.Error(), tc.path+": must contain at most") {
				t.Fatalf("missing bound: %v", err)
			}
		})
	}
}
