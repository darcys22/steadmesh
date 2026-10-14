package tools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/store"
)

func TestConnectionListingPreservesScopeClauses(t *testing.T) {
	seat := &store.Seat{Manifest: compile.SeatManifest{Capabilities: []compile.Capability{
		{Resource: "connection:github", Operations: []string{"issue.comment"}, Targets: []string{"acme/b"}},
		{Resource: "connection:github", Operations: []string{"issue.read"}, Targets: []string{"acme/a"}},
		{Resource: "connection:github", Operations: []string{"repo.read"}},
	}}}
	org := &store.Organization{Manifest: compile.Manifest{Spec: spec.OrganizationSpec{Connections: map[string]spec.Connection{"github": {Adapter: "github"}}}}}
	r := &Registry{}
	out, err := r.connectionsList(context.Background(), &Call{Seat: seat, Org: org})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Connections []struct {
			Key                 string
			Operations, Targets []string
		}
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Connections) != 3 {
		t.Fatalf("dropped clauses: %s", b)
	}
	for i, want := range []struct{ op, target string }{{"repo.read", ""}, {"issue.read", "acme/a"}, {"issue.comment", "acme/b"}} {
		c := got.Connections[i]
		if c.Key != "github" || !slices.Equal(c.Operations, []string{want.op}) || (want.target == "" && len(c.Targets) != 0) || (want.target != "" && !slices.Equal(c.Targets, []string{want.target})) {
			t.Fatalf("clause %d lost pairing: %s", i, b)
		}
	}
	self := Self(seat, org)
	if len(self.Connections) != 1 || !slices.Equal(self.Connections[0].Operations, []string{"issue.comment", "issue.read", "repo.read"}) {
		t.Fatalf("self summary dropped operations: %+v", self.Connections)
	}
}

func TestMemorySummaryDeduplicatesClauses(t *testing.T) {
	sm := &compile.SeatManifest{PersonalMemory: "a", Capabilities: []compile.Capability{
		{Resource: "memory:b", Operations: []string{"read"}},
		{Resource: "memory:a", Operations: []string{"read"}},
		{Resource: "memory:a", Operations: []string{"read", "write"}},
	}}
	got := memoryStoreInfos(sm)
	if len(got) != 2 || got[0].Key != "a" || !got[0].Personal || !slices.Equal(got[0].Operations, []string{"read", "write"}) || got[1].Key != "b" {
		t.Fatalf("memory summary: %+v", got)
	}
	if !slices.Equal(sm.Capabilities[1].Operations, []string{"read"}) {
		t.Fatal("summary mutated capability clauses")
	}
}
