package compile

import (
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestRequiredConnectionsByRole(t *testing.T) {
	no := false
	m := &Manifest{
		Spec: spec.OrganizationSpec{
			Connections: map[string]spec.Connection{
				"slack":   {Adapter: "slack"},
				"chat":    {Adapter: "slack"}, // no bindings
				"model":   {Adapter: "anthropic"},
				"spare":   {Adapter: "anthropic"}, // unused
				"linear":  {Adapter: "linear"},
				"opt_out": {Adapter: "anthropic", Required: &no},
			},
			ChannelBindings: map[string]spec.ChannelBinding{"alice": {Connection: "slack", Seat: "rep"}},
		},
		Seats: map[string]SeatManifest{
			"rep": {Harness: spec.HarnessProfile{ModelConnection: "model"}},
			"x":   {Harness: spec.HarnessProfile{ModelConnection: "opt_out"}},
		},
	}
	got := RequiredConnections(m, DefaultCatalog())
	want := map[string]bool{"slack": true, "chat": false, "model": true, "spare": false, "linear": false, "opt_out": false}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: required = %v, want %v", k, got[k], w)
		}
	}
}
