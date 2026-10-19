package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
	"strings"
	"testing"
)

func TestReviewBindsMaterialWork(t *testing.T) {
	w := workItem{Objective: "ship", Owner: "a", Acceptance: []string{"tests"}, Evidence: []string{"passed"}}
	w.Review = &workReview{Reviewer: "b", Digest: w.reviewDigest(), Revision: 2}
	if !w.reviewed() {
		t.Fatal("fresh review not valid")
	}
	w.note("c", "FYI")
	if !w.reviewed() {
		t.Fatal("comment invalidated deliverable")
	}
	w.Evidence = append(w.Evidence, "changed")
	if w.reviewed() {
		t.Fatal("changed evidence kept approval")
	}
	w.Review.Digest = w.reviewDigest()
	w.Review.Reviewer = w.Owner
	if w.reviewed() {
		t.Fatal("self review accepted")
	}
}
func TestTeamDiscoveryAndWorkDenyOutsideMembers(t *testing.T) {
	org := &store.Organization{Manifest: compile.Manifest{Spec: spec.OrganizationSpec{Teams: map[string]spec.Team{"a": {Coordination: &spec.TeamCoordination{WorkStore: "board"}}, "b": {Coordination: &spec.TeamCoordination{}}}}}}
	c := &Call{Seat: &store.Seat{Manifest: compile.SeatManifest{Key: "member", Teams: []string{"b"}}}, Org: org, Args: json.RawMessage(`{"team":"a"}`)}
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	if _, err := r.teamDiscover(context.Background(), c); err == nil {
		t.Fatal("cross-team directory query reached store")
	}
	if err := checkTeamWork(c, "board"); err == nil {
		t.Fatal("cross-team board allowed")
	}
	if nativeTeamAllowed(&compile.SeatManifest{}, &org.Manifest) {
		t.Fatal("legacy seat gained new tools")
	}
}

func TestNativeParticipantsCannotSpawnLegacyChildren(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	seat := &store.Seat{Manifest: compile.SeatManifest{Key: "pool_one", PoolTeam: "engineering"}}
	org := &store.Organization{Manifest: compile.Manifest{Spec: spec.OrganizationSpec{ElasticTeams: &spec.ElasticTeams{EnabledSeats: []string{"pool_one"}}}}}
	for _, d := range r.List(seat, org) {
		if d.Name == "tasks.start" || d.Name == "branches.start" || d.Name == "teams.expand" {
			t.Fatalf("native actor offered child tool %s", d.Name)
		}
	}
}

func TestPoolPagesFitAndReachEveryMember(t *testing.T) {
	members := make([]runtimeapi.PoolMember, 200)
	for i := range members {
		members[i] = runtimeapi.PoolMember{SeatID: fmt.Sprint(i), SeatKey: strings.Repeat("s", 80), Team: strings.Repeat("t", 80), Template: strings.Repeat("x", 80)}
	}
	cursor, total := "", 0
	for {
		out, err := poolPage("team", members, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(out)
		if err != nil || len(raw) > MaxResultBytes {
			t.Fatalf("page too large %d %v", len(raw), err)
		}
		m := out.(map[string]any)
		page := m["members"].([]runtimeapi.PoolMember)
		if len(page) == 0 {
			t.Fatal("empty page before completion")
		}
		total += len(page)
		cursor = m["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if total != len(members) {
		t.Fatalf("got %d of%d", total, len(members))
	}
}
