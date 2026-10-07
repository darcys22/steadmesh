//go:build integration

package codex

import (
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/harnesses/conformance/modelstub"
)

// TestModelConformance runs the pinned Codex app-server against a scripted
// OpenAI Responses endpoint through the real model forwarder.
//
//	build/harness-bins.sh codex && go test -tags integration -run ModelConformance ./harnesses/codex/
func TestModelConformance(t *testing.T) {
	bin := conformance.HarnessBin(t, "codex")
	// gpt-5.5 is in Codex's model catalog with tool search: MCP tools are
	// deferred and the model finds them with tool_search first, as with the
	// real OpenAI API. An unknown model is offered the tools directly.
	for _, model := range []string{"gpt-5.5", "gpt-stub-1"} {
		t.Run(model, func(t *testing.T) {
			conformance.RunModel(t, conformance.ModelOptions{
				New:      func() harnesses.Adapter { return New() },
				API:      harnesses.APIOpenAIResponses,
				Model:    model,
				Settings: map[string]string{SettingReasoningEffort: "low"},
				Configure: func(t *testing.T, env *harnesses.Environment) {
					env.HarnessConfig = map[string]string{ConfigBinary: bin, ConfigVersion: PinnedVersion, ConfigInterruptGrace: "10s"}
				},
				CheckToolCall: func(t *testing.T, reqs []modelstub.Request) {
					searched := false
					for _, r := range reqs {
						searched = searched || r.Searches > 0
					}
					if want := model == "gpt-5.5"; searched != want {
						t.Fatalf("tool_search used: %v, want %v", searched, want)
					}
				},
			})
		})
	}
}
