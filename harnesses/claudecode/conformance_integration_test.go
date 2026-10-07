//go:build integration

package claudecode

import (
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
)

// TestModelConformance runs the pinned Claude Code CLI against a scripted
// Anthropic Messages endpoint through the real model forwarder. HOME is a
// temp dir and --bare keeps the developer's login out of the run.
//
//	build/harness-bins.sh claude && go test -tags integration -run ModelConformance ./harnesses/claudecode/
func TestModelConformance(t *testing.T) {
	bin := conformance.HarnessBin(t, "claude")
	conformance.RunModel(t, conformance.ModelOptions{
		New:      func() harnesses.Adapter { return New() },
		API:      harnesses.APIAnthropicMessages,
		Model:    "claude-stub-1",
		Settings: map[string]string{SettingEffort: "low"},
		Configure: func(t *testing.T, env *harnesses.Environment) {
			env.HarnessConfig = map[string]string{ConfigBinary: bin, ConfigVersion: PinnedVersion, ConfigInterruptGrace: "5s"}
		},
	})
}
