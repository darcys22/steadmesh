package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

func TestPageCursor(t *testing.T) {
	off, n, err := page(cursorAt(40), 0, 10, 25)
	if err != nil || off != 40 || n != 10 {
		t.Fatalf("page = %d %d %v", off, n, err)
	}
	if _, n, _ := page("", 500, 10, 25); n != 25 {
		t.Fatalf("limit not clamped: %d", n)
	}
	if _, _, err := page("!!", 0, 10, 25); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func TestFitBoundsResults(t *testing.T) {
	items := make([]string, 50)
	for i := range items {
		items[i] = strings.Repeat("x", 1000)
	}
	out := fit(items, 0, false, func(v []string, next string) any { return map[string]any{"items": v, "next_cursor": next} })
	b, _ := json.Marshal(out)
	if len(b) > MaxResultBytes {
		t.Fatalf("result is %d bytes", len(b))
	}
	m := out.(map[string]any)
	got := len(m["items"].([]string))
	off, _, _ := page(m["next_cursor"].(string), 0, 1, 1)
	if got == 0 || got == 50 || off != got {
		t.Fatalf("kept %d items, cursor offset %d", got, off)
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	s := strings.Repeat("é", 10)
	got, cut := truncate(s, 5)
	if !cut || !utf8.ValidString(got) || len(got) != 4 {
		t.Fatalf("truncate = %q %v", got, cut)
	}
}

func TestDescriptorsAreFilteredAndValid(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	m := orgfixture.Manifest(t)
	org := &store.Organization{Manifest: *m}
	names := func(key string) map[string]bool {
		sm := m.Seats[key]
		out := map[string]bool{}
		for _, d := range r.List(&store.Seat{Key: key, Manifest: sm}, org) {
			var schema map[string]any
			if err := json.Unmarshal(d.InputSchema, &schema); err != nil || schema["type"] != "object" {
				t.Fatalf("%s: invalid schema: %v", d.Name, err)
			}
			out[d.Name] = true
		}
		return out
	}
	lead, reviewer := names("lead"), names("reviewer")
	for _, n := range []string{"connections.invoke", "operations.get", "messages.send", "memory.publish"} {
		if !lead[n] {
			t.Errorf("lead lacks %s", n)
		}
	}
	for _, n := range []string{"connections.invoke", "connections.list", "messages.send"} {
		if reviewer[n] {
			t.Errorf("reviewer offered %s", n)
		}
	}
	if !reviewer["self"] || !reviewer["memory.search"] || !reviewer["handoff.update"] {
		t.Errorf("reviewer lacks basic tools: %v", reviewer)
	}
	// Work tools need a shared store; the representative has only personal memory.
	if !lead["work.create"] || !lead["work.claim"] || !lead["memory.append"] || !lead["messages.inbox"] {
		t.Errorf("lead lacks work, append or inbox tools: %v", lead)
	}
	if rep := names("rep_b"); rep["work.create"] || rep["work.list"] {
		t.Errorf("seat without a shared store offered work tools: %v", rep)
	}
	if len(r.tools) != 30 {
		t.Errorf("registry has %d tools, want the 30 of docs/tools.html", len(r.tools))
	}
}

func TestCodeHostConnectionsAreInvocable(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	s := orgfixture.Spec()
	s.Connections["github"] = spec.Connection{Adapter: "github", SecretRef: "k8s:github"}
	s.AccessProfiles = map[string]spec.AccessProfile{"gh": {GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"pull_requests": "write"}}}}
	rv := s.Seats["reviewer"]
	rv.AccessProfiles = []string{"gh"}
	s.Seats["reviewer"] = rv
	m := orgfixture.Compile(t, s)
	got := map[string]bool{}
	for _, d := range r.List(&store.Seat{Key: "reviewer", Manifest: m.Seats["reviewer"]}, &store.Organization{Manifest: *m}) {
		got[d.Name] = true
	}
	if !got["connections.invoke"] || !got["operations.get"] {
		t.Fatalf("a seat granted GitHub operations is not offered connections.invoke: %v", got)
	}
}
