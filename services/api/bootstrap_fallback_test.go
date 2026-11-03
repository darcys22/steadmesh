package api

import (
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestBootstrapPreservesPersistedRoute(t *testing.T) {
	primary := runtimeapi.Checkpoint{HarnessAdapter: "codex", HarnessProfile: "primary", RouteBinding: "primary-binding", CheckpointRef: "old-primary"}
	secondary := runtimeapi.Checkpoint{HarnessAdapter: "pi", HarnessProfile: "secondary", RouteBinding: "secondary-binding", CheckpointRef: "active-secondary"}
	legacy := runtimeapi.Checkpoint{HarnessAdapter: "codex", CheckpointRef: "legacy"}
	for _, tc := range []struct {
		name        string
		fallback    bool
		checkpoints []runtimeapi.Checkpoint
		want        *runtimeapi.Checkpoint
	}{
		{"secondary survives restart", true, []runtimeapi.Checkpoint{secondary, primary}, &secondary},
		{"secondary survives revocation", false, []runtimeapi.Checkpoint{secondary, primary}, &secondary},
		{"changed binding reaches runner", true, []runtimeapi.Checkpoint{secondary}, &secondary},
		{"bound route not shadowed by legacy adapter match", false, []runtimeapi.Checkpoint{legacy, secondary}, &secondary},
		{"newest bound route wins", true, []runtimeapi.Checkpoint{primary, secondary}, &primary},
		{"legacy same adapter unchanged", false, []runtimeapi.Checkpoint{legacy}, &legacy},
		{"no checkpoints", true, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := compile.SeatManifest{HarnessProfile: "primary", Harness: spec.HarnessProfile{Adapter: "codex"}}
			if tc.fallback {
				m.Harness.StartupFallback = &spec.StartupFallbackPolicy{Profiles: []string{"secondary"}, MaxAttempts: 2, TotalTimeout: "30s"}
			}
			got := bootstrapCheckpoint(&m, tc.checkpoints)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestBootstrapDoesNotDiscardLegacySessionWhenEnablingFallback(t *testing.T) {
	m := compile.SeatManifest{Harness: spec.HarnessProfile{Adapter: "codex", StartupFallback: &spec.StartupFallbackPolicy{}}}
	cp := runtimeapi.Checkpoint{HarnessAdapter: "pi", CheckpointRef: "legacy-secondary"}
	if got := bootstrapCheckpoint(&m, []runtimeapi.Checkpoint{cp}); got == nil || *got != cp {
		t.Fatal("legacy cross-adapter session disappeared instead of reaching runner migration guard")
	}
	m.Harness.StartupFallback = nil
	if got := bootstrapCheckpoint(&m, []runtimeapi.Checkpoint{cp}); got != nil {
		t.Fatal("ordinary legacy cross-adapter behavior changed")
	}
}

func TestBootstrapPreservesIncompleteRouteForFailClosedValidation(t *testing.T) {
	m := compile.SeatManifest{Harness: spec.HarnessProfile{Adapter: "codex"}}
	for _, cp := range []runtimeapi.Checkpoint{
		{HarnessAdapter: "pi", HarnessProfile: "secondary"},
		{HarnessAdapter: "pi", RouteBinding: "binding"},
	} {
		if got := bootstrapCheckpoint(&m, []runtimeapi.Checkpoint{cp}); got == nil || *got != cp {
			t.Fatal("incomplete route silently discarded")
		}
	}
}
