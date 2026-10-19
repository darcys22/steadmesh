package compile

import (
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/spec"
)

// RequiredConnections reports, per declared connection, whether it must
// authenticate before the organisation is operationally ready. An explicit
// `required` wins. Otherwise a connection is required when the organisation
// cannot work without it: a model connection a seat's harness uses, or a
// communication connection carrying channel bindings. Everything else, such
// as a work tracker, is optional: its failures are reported but never block
// readiness or internal work.
func RequiredConnections(m *Manifest, cat Catalog) map[string]bool {
	used := map[string]bool{}
	for connection := range ModelUses(m) {
		used[connection] = true
	}
	for _, b := range m.Spec.ChannelBindings {
		used[b.Connection] = true
	}
	out := map[string]bool{}
	for k, c := range m.Spec.Connections {
		switch {
		case c.Required != nil:
			out[k] = *c.Required
		default:
			kind := cat.Connectors[c.Adapter].Kind
			out[k] = used[k] && (kind == "model" || kind == "communication")
		}
	}
	return out
}

// ModelUse is a model a seat requests over an API.
type ModelUse struct {
	ID  string
	API string
}

// ModelUses lists, per model connection, the models and APIs the seats'
// harness profiles and declared native pool templates use, sorted. Readiness checks each of them.
func ModelUses(m *Manifest) map[string][]ModelUse {
	out := map[string][]ModelUse{}
	add := func(sel *spec.ModelSelection) {
		if sel == nil {
			return
		}
		u := ModelUse{ID: sel.ID, API: sel.API}
		if !slices.Contains(out[sel.Connection], u) {
			out[sel.Connection] = append(out[sel.Connection], u)
		}
	}
	for _, k := range sortedKeys(m.Seats) {
		add(m.Seats[k].Harness.Model)
	}
	// Declared templates need verification even before the first admission.
	// This never inserts a runtime participant in desired-state Seats.
	for _, tk := range sortedKeys(m.Spec.Teams) {
		if pool := m.Spec.Teams[tk].Pool; pool != nil {
			for _, name := range sortedKeys(pool.Templates) {
				if seat := pool.Templates[name].Seat; seat != nil {
					add(m.Spec.HarnessProfiles[seat.HarnessProfile].Model)
				}
			}
		}
	}
	for _, us := range out {
		slices.SortFunc(us, func(a, b ModelUse) int { return strings.Compare(a.ID+"\x00"+a.API, b.ID+"\x00"+b.API) })
	}
	return out
}
