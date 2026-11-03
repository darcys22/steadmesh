package api

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// checkpointRouteAllowed binds an opted-in route to the current committed
// manifest. A stale pod cannot persist a revoked profile or modified model.
func checkpointRouteAllowed(m *compile.SeatManifest, cp runtimeapi.Checkpoint) bool {
	if cp.HarnessProfile == "" && cp.RouteBinding == "" {
		return m.Harness.StartupFallback == nil && len(m.StartupFallbacks) == 0
	}
	candidates := []compile.StartupFallbackCandidate{{Profile: m.HarnessProfile, Harness: m.Harness}}
	candidates = append(candidates, m.StartupFallbacks...)
	for _, candidate := range candidates {
		if candidate.Profile == cp.HarnessProfile && candidate.Harness.Adapter == cp.HarnessAdapter && compile.StartupCandidateBinding(candidate.Profile, candidate.Harness) == cp.RouteBinding {
			return true
		}
	}
	return false
}
