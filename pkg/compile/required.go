package compile

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
		if s.Harness.ModelConnection != "" {
			used[s.Harness.ModelConnection] = true
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
