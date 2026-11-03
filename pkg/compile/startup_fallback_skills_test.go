package compile_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestStartupFallbackPreservesManagedSkillCapability(t *testing.T) {
	s := startupFallbackSpec(t)
	cat := compile.DefaultCatalog()
	secondary := cat.Harnesses["codex"]
	secondary.Capabilities = slices.DeleteFunc(slices.Clone(secondary.Capabilities), func(c string) bool { return c == harnesses.CapabilityManagedSkills })
	cat.Harnesses["codex"] = secondary
	// The alternate remains valid for a seat without marketplace skills.
	if _, err := compile.Compile(s, cat); err != nil {
		t.Fatal(err)
	}
	s.SkillMarketplaces = map[string]spec.SkillMarketplace{"approved": {Repository: "example/skills", Ref: strings.Repeat("a", 40)}}
	seat := s.Seats["reviewer"]
	seat.Skills = &spec.SkillPolicy{Marketplaces: []string{"approved"}}
	s.Seats["reviewer"] = seat
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
	m, err := compile.Compile(s, cat)
	if err == nil || m != nil || !strings.Contains(err.Error(), `startup fallback profile "secondary"`) || !strings.Contains(err.Error(), harnesses.CapabilityManagedSkills) {
		t.Fatalf("fallback silently dropped managed skills: manifest=%v error=%v", m, err)
	}
}

func TestStartupFallbackChecksEveryApprovedSkillCandidate(t *testing.T) {
	s := startupFallbackSpec(t)
	s.HarnessProfiles["h"].StartupFallback.MaxAttempts = 1
	s.SkillMarketplaces = map[string]spec.SkillMarketplace{"approved": {Repository: "example/skills", Ref: strings.Repeat("a", 40)}}
	seat := s.Seats["reviewer"]
	seat.Skills = &spec.SkillPolicy{Marketplaces: []string{"approved"}}
	s.Seats["reviewer"] = seat
	cat := compile.DefaultCatalog()
	last := cat.Harnesses["pi"]
	last.Capabilities = slices.DeleteFunc(slices.Clone(last.Capabilities), func(c string) bool { return c == harnesses.CapabilityManagedSkills })
	cat.Harnesses["pi"] = last
	if _, err := compile.Compile(s, cat); err == nil || !strings.Contains(err.Error(), `startup fallback profile "third"`) {
		t.Fatalf("declared candidate beyond current attempt prefix escaped validation: %v", err)
	}
}
