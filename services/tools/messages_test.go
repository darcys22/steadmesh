package tools

import (
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

func TestExternalRepresentativeReplyNeedsNoHumanBindingOrRoute(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	org := &store.Organization{Manifest: compile.Manifest{}}
	seat := &store.Seat{Manifest: compile.SeatManifest{IsRepresentative: true}}
	names := map[string]bool{}
	for _, d := range r.List(seat, org) {
		names[d.Name] = true
	}
	if !names["messages.reply"] {
		t.Fatal("external representative lacks replies without human binding")
	}
	if names["messages.send"] {
		t.Fatal("external representative unexpectedly gained a swarm route")
	}
	seat.Manifest.IsRepresentative = false
	for _, d := range r.List(seat, org) {
		if d.Name == "messages.reply" {
			t.Fatal("unconnected non-representative gained replies")
		}
	}
}
