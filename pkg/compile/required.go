package compile

import (
	"slices"
	"strings"
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
	for _, s := range m.Seats {
		if s.Harness.Model != nil {
			used[s.Harness.Model.Connection] = true
		}
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
// harness profiles use, sorted. Readiness checks each of them.
func ModelUses(m *Manifest) map[string][]ModelUse {
	out := map[string][]ModelUse{}
	for _, k := range sortedKeys(m.Seats) {
		sel := m.Seats[k].Harness.Model
		if sel == nil {
			continue
		}
		u := ModelUse{ID: sel.ID, API: sel.API}
		if !slices.Contains(out[sel.Connection], u) {
			out[sel.Connection] = append(out[sel.Connection], u)
		}
	}
	for _, us := range out {
		slices.SortFunc(us, func(a, b ModelUse) int { return strings.Compare(a.ID+"\x00"+a.API, b.ID+"\x00"+b.API) })
	}
	return out
}
