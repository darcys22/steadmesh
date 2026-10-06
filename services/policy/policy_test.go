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
