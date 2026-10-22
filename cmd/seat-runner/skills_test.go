package main

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestEnvironmentSkillLoaderOnlyForConfiguredSeats(t *testing.T) {
	r := &Runner{c: &client.Client{}, cfg: Config{RunnerDir: t.TempDir()}}
	for _, m := range []*compile.SeatManifest{nil, {}, {Skills: &spec.SkillPolicy{Mode: spec.SkillModeExplicit}}} {
		got := r.environment(runtimeapi.Bootstrap{}, m, "")
		want := m != nil && m.Skills != nil
		if (got.LoadSkills != nil) != want {
			t.Fatalf("LoadSkills configured=%v, want %v", got.LoadSkills != nil, want)
		}
	}
}
