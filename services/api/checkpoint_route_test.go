package api

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestCheckpointRouteAllowlist(t *testing.T) {
	primary := spec.HarnessProfile{Adapter: "codex", Model: &spec.ModelSelection{Connection: "a", ID: "one", API: "openai_responses"}, StartupFallback: &spec.StartupFallbackPolicy{}}
	secondary := spec.HarnessProfile{Adapter: "pi", Model: &spec.ModelSelection{Connection: "b", ID: "two", API: "openai_chat"}}
	m := &compile.SeatManifest{HarnessProfile: "primary", Harness: primary, StartupFallbacks: []compile.StartupFallbackCandidate{{Profile: "secondary", Harness: secondary}}}
	cp := runtimeapi.Checkpoint{HarnessProfile: "secondary", HarnessAdapter: "pi", RouteBinding: compile.StartupCandidateBinding("secondary", secondary)}
	if !checkpointRouteAllowed(m, cp) {
		t.Fatal("approved route denied")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*runtimeapi.Checkpoint)
	}{
		{"revoked profile", func(cp *runtimeapi.Checkpoint) { cp.HarnessProfile = "unapproved" }},
		{"wrong adapter", func(cp *runtimeapi.Checkpoint) { cp.HarnessAdapter = "codex" }},
		{"changed binding", func(cp *runtimeapi.Checkpoint) { cp.RouteBinding = "stale" }},
		{"legacy bypass", func(cp *runtimeapi.Checkpoint) { cp.HarnessProfile = ""; cp.RouteBinding = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := cp
			tc.mutate(&bad)
			if checkpointRouteAllowed(m, bad) {
				t.Fatal("invalid route allowed")
			}
		})
	}
	m.StartupFallbacks = nil
	if checkpointRouteAllowed(m, cp) {
		t.Fatal("revoked secondary route allowed")
	}
	m.Harness.StartupFallback = nil
	if !checkpointRouteAllowed(m, runtimeapi.Checkpoint{}) {
		t.Fatal("legacy behavior changed without fallback")
	}
}
