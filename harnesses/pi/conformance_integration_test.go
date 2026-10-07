//go:build integration

package pi

import (
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
)

// TestModelConformance runs the pinned Pi CLI against a scripted endpoint
// for each API it speaks, through the real model forwarder.
//
//	build/harness-bins.sh pi && go test -tags integration -run ModelConformance ./harnesses/pi/
func TestModelConformance(t *testing.T) {
	bin := conformance.HarnessBin(t, "pi")
	for _, api := range []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat, harnesses.APIAnthropicMessages} {
		t.Run(api, func(t *testing.T) {
			conformance.RunModel(t, conformance.ModelOptions{
				New:      func() harnesses.Adapter { return New() },
				API:      api,
				Model:    "stub-" + api,
				Settings: map[string]string{SettingContextWindow: "64000", SettingMaxTokens: "4096"},
				Configure: func(t *testing.T, env *harnesses.Environment) {
					env.HarnessConfig = map[string]string{ConfigBinary: bin, ConfigVersion: PinnedVersion, ConfigInterruptGrace: "10s"}
				},
			})
		})
	}
}
