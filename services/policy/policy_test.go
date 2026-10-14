package policy_test

import (
	"slices"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/policy"
)

func seat(t *testing.T, key string) *compile.SeatManifest {
	t.Helper()
	sm := orgfixture.Manifest(t).Seats[key]
	return &sm
}

func TestMemoryStores(t *testing.T) {
	repA := seat(t, "rep_a")
	if got := policy.MemoryStores(repA, "search"); !slices.Equal(got, []string{"organisation", "rep_a"}) {
		t.Fatalf("rep_a search stores = %v", got)
	}
	if got := policy.MemoryStores(repA, "write"); !slices.Equal(got, []string{"rep_a"}) {
		t.Fatalf("rep_a write stores = %v", got)
	}
	if policy.Allows(repA, "memory:rep_b", "read") {
		t.Fatal("rep_a may read rep_b's personal memory")
	}
	if !policy.Allows(seat(t, "engineer"), "memory:engineering", "revise") {
		t.Fatal("team shared memory not granted")
	}
}

func TestConnectionAllowed(t *testing.T) {
	lead := seat(t, "lead")
	if !policy.ConnectionAllowed(lead, "tracker", "project.create", "") {
		t.Fatal("granted operation denied")
	}
	if policy.ConnectionAllowed(lead, "tracker", "comment.write", "") {
		t.Fatal("ungranted operation allowed")
	}
	if policy.ConnectionAllowed(seat(t, "engineer"), "tracker", "project.read", "") {
		t.Fatal("seat without grant allowed")
	}

	sp := orgfixture.Spec()
	g := sp.Grants["lead_tracker"]
	g.Targets = []string{"TEAM-1", "proj-*"}
	sp.Grants["lead_tracker"] = g
	lead2 := orgfixture.Compile(t, sp).Seats["lead"]
	for target, want := range map[string]bool{"TEAM-1": true, "proj-42": true, "TEAM-2": false, "": false} {
		if got := policy.ConnectionAllowed(&lead2, "tracker", "project.read", target); got != want {
			t.Errorf("target %q: got %v want %v", target, got, want)
		}
	}
}

func TestRoutesAndReplies(t *testing.T) {
	repA, lead, eng, reviewer := seat(t, "rep_a"), seat(t, "lead"), seat(t, "engineer"), seat(t, "reviewer")
	if _, ok := policy.SendEdge(repA, "lead"); !ok {
		t.Fatal("declared route missing")
	}
	if _, ok := policy.SendEdge(repA, "engineer"); ok {
		t.Fatal("undeclared route allowed")
	}
	if !policy.CanReplyToSeat(lead, "rep_a") {
		t.Fatal("reply=true route does not permit reply")
	}
	if !policy.CanReplyToSeat(eng, "lead") {
		t.Fatal("bidirectional route does not permit reply")
	}
	if policy.CanReplyToSeat(reviewer, "engineer") {
		t.Fatal("route without reply permits reply")
	}
	if !policy.HasBinding(repA, "alice") || policy.HasBinding(repA, "bob") {
		t.Fatal("binding ownership wrong")
	}
	if got := policy.Reachable(lead); !slices.Equal(got, []string{"engineer", "rep_a", "rep_b"}) {
		t.Fatalf("reachable = %v", got)
	}
}

func TestConnectionClausesAreDisjunctions(t *testing.T) {
	sm := &compile.SeatManifest{Capabilities: []compile.Capability{
		{Resource: "connection:github", Operations: []string{"read"}, Targets: []string{"acme/a", "acme/shared-*"}},
		{Resource: "connection:github", Operations: []string{"write"}, Targets: []string{"acme/b"}},
		{Resource: "connection:github", Operations: []string{"read"}, Targets: []string{"acme/c"}},
		{Resource: "connection:github", Operations: []string{"metadata"}},
	}}
	for _, tt := range []struct {
		op      string
		targets []string
		want    bool
	}{
		{"read", []string{"acme/a", "acme/c"}, true},
		{"read", []string{"acme/a", "acme/shared-one"}, true},
		{"read", []string{"acme/a", "acme/b"}, false},
		{"write", []string{"acme/b"}, true},
		{"write", []string{"acme/a"}, false},
		{"write", nil, false}, {"read", nil, false},
		{"metadata", nil, true}, {"metadata", []string{"acme/anything"}, true},
		{"metadata", []string{""}, false}, {"unknown", nil, false},
	} {
		if got := policy.ConnectionTargetsAllowed(sm, "github", tt.op, tt.targets); got != tt.want {
			t.Errorf("%s %v: got %v want %v", tt.op, tt.targets, got, tt.want)
		}
	}
	if _, ok := policy.Capability(sm, "connection:github"); ok {
		t.Fatal("ambiguous capability returned one clause")
	}
	if !policy.Allows(sm, "connection:github", "write") {
		t.Fatal("later clause ignored")
	}
	if len(policy.Connections(sm)["github"]) != 4 {
		t.Fatal("connection listing dropped clauses")
	}
}

func TestMemoryStoresDeduplicatesClauses(t *testing.T) {
	sm := &compile.SeatManifest{Capabilities: []compile.Capability{
		{Resource: "memory:b", Operations: []string{"read"}},
		{Resource: "memory:a", Operations: []string{"read"}},
		{Resource: "memory:a", Operations: []string{"read", "write"}},
	}}
	if got := policy.MemoryStores(sm, "read"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if !policy.Allows(sm, "memory:a", "write") {
		t.Fatal("later memory clause ignored")
	}
}
