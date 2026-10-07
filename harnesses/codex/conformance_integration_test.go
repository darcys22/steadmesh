//go:build integration

package codex

import (
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
)

// TestModelConformance runs the pinned Codex app-server against a scripted
// OpenAI Responses endpoint through the real model forwarder.
//
//	build/harness-bins.sh codex && go test -tags integration -run ModelConformance ./harnesses/codex/
func TestModelConformance(t *testing.T) {
	bin := conformance.HarnessBin(t, "codex")
	conformance.RunModel(t, conformance.ModelOptions{
		New:      func() harnesses.Adapter { return New() },
		API:      harnesses.APIOpenAIResponses,
		Model:    "gpt-stub-1",
		Settings: map[string]string{SettingReasoningEffort: "low"},
		Configure: func(t *testing.T, env *harnesses.Environment) {
			env.HarnessConfig = map[string]string{ConfigBinary: bin, ConfigVersion: PinnedVersion, ConfigInterruptGrace: "10s"}
		},
	})
}
