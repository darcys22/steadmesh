package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func skillTestManifest(mode string) compile.SeatManifest {
	return compile.SeatManifest{Skills: &spec.SkillPolicy{Mode: mode, Marketplaces: []string{"allowed"}, MaxDynamicSkills: 1}, SkillMarketplaces: map[string]spec.SkillMarketplace{"allowed": {Repository: "acme/skills", Ref: strings.Repeat("a", 40)}}}
}
func skillTestRef(name string) spec.SkillRef {
	return spec.SkillRef{Marketplace: "allowed", Plugin: "plugin", Skill: name}
}

func TestEffectiveSkillsPolicyAndProvenance(t *testing.T) {
	sm := skillTestManifest("agent_choice")
	static, dynamic := skillTestRef("static"), skillTestRef("dynamic")
	sm.Skills.Install = []spec.SkillRef{static}
	source := sm.SkillMarketplaces["allowed"]
	selected := []SkillSelection{{Ref: static, Source: source}, {Ref: dynamic, Source: source}}
	for _, tc := range []struct {
		mode string
		want int
	}{{"explicit", 1}, {"discover", 1}, {"agent_choice", 2}} {
		sm.Skills.Mode = tc.mode
		got, err := effectiveSkills(&sm, selected)
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s: %+v %v", tc.mode, got, err)
		}
		for _, v := range got {
			if v.Ref == static && v.Owner != "terraform" {
				t.Fatalf("static ownership: %+v", v)
			}
		}
	}
	sm.Skills.Mode = "agent_choice"
	changed := source
	changed.Ref = strings.Repeat("b", 40)
	sm.SkillMarketplaces["allowed"] = changed
	got, err := effectiveSkills(&sm, selected)
	if err != nil || len(got) != 1 || got[0].Source != changed {
		t.Fatalf("source replacement: %+v %v", got, err)
	}
	sm.Skills.Marketplaces = nil
	if _, err := effectiveSkills(&sm, selected); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid static must fail closed: %v", err)
	}
	sm.Skills = nil
	got, err = effectiveSkills(&sm, selected)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("disabled did not clear: %+v %v", got, err)
	}
}

func TestEffectiveSkillsBoundsAndRevocation(t *testing.T) {
	sm := skillTestManifest("agent_choice")
	source := sm.SkillMarketplaces["allowed"]
	selected := []SkillSelection{{Ref: skillTestRef("z"), Source: source}, {Ref: skillTestRef("a"), Source: source}}
	got, err := effectiveSkills(&sm, selected)
	if err != nil || len(got) != 1 || got[0].Ref.Skill != "a" {
		t.Fatalf("deterministic cap: %+v %v", got, err)
	}
	delete(sm.SkillMarketplaces, "allowed")
	got, err = effectiveSkills(&sm, selected)
	if err != nil || len(got) != 0 {
		t.Fatalf("revoked source: %+v %v", got, err)
	}
	sm.Skills.Mode = "anything"
	if _, err := effectiveSkills(&sm, selected); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown mode accepted: %v", err)
	}
}

func TestSkillFingerprintAndSafeReferences(t *testing.T) {
	a := spec.SkillMarketplace{Repository: "acme/one", Ref: strings.Repeat("a", 40)}
	b := a
	b.Repository = "acme/two"
	if len(SkillSourceFingerprint(a)) != 64 || SkillSourceFingerprint(a) == SkillSourceFingerprint(b) {
		t.Fatal("source provenance not distinct")
	}
	b = a
	b.Ref = strings.Repeat("b", 40)
	if SkillSourceFingerprint(a) == SkillSourceFingerprint(b) {
		t.Fatal("commit provenance not distinct")
	}
	for _, value := range []string{"", "../x", "UPPER", "a/b", strings.Repeat("a", 129)} {
		r := skillTestRef(value)
		if ValidSkillRef(r) {
			t.Fatalf("unsafe ref accepted: %+v", r)
		}
	}
}
