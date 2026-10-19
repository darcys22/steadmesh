//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func coordinatedFixture(t *testing.T, s *Store) (string, map[string]string) {
	t.Helper()
	sp := orgfixture.Spec()
	team := sp.Teams["engineering"]
	team.Coordination = &spec.TeamCoordination{Mode: "self_organising", Mission: "Ship a tested result", WorkStore: "engineering", MissionEditors: []string{"seat:lead"}, ReviewPolicy: "required", MaxMessagesPerExecution: 3}
	sp.Teams["engineering"] = team
	sp.MessageRoutes["peer"] = spec.MessageRoute{From: "team:engineering", To: "team:engineering", Reply: true}
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	ids := map[string]string{}
	for key, id := range o.Seats {
		ids[key] = id.SeatID
	}
	return o.OrganizationID, ids
}
func TestTeamMissionAndDiscoveryMembership(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, ids := coordinatedFixture(t, s)
	lead := lease(t, s, ids["lead"])
	initial, e := s.TeamMission(ctx, lead.SeatID, "engineering")
	if e != nil {
		t.Fatal(e)
	}
	other := lease(t, s, ids["engineer"])
	peers, err := s.TeamPeers(ctx, lead.SeatID, "engineering", "", 50)
	if err != nil || len(peers) != 3 {
		t.Fatalf("peers: %+v %v", peers, err)
	}
	if _, err = s.TeamPeers(ctx, ids["rep_b"], "engineering", "", 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonmember discover: %v", err)
	}
	if _, err = s.UpdateTeamMission(ctx, other, "engineering", 0, initial.PolicyRevision, "take over", "because"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unconfigured editor: %v", err)
	}
	var wg sync.WaitGroup
	var wins, conflicts atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdateTeamMission(ctx, lead, "engineering", 0, initial.PolicyRevision, "Refined mission", "evidence")
			if err == nil {
				wins.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("CAS wins %d conflicts %d", wins.Load(), conflicts.Load())
	}
	m, err := s.TeamMission(ctx, other.SeatID, "engineering")
	if err != nil || m.Revision != 1 || m.Author != "lead" {
		t.Fatalf("mission: %+v %v", m, err)
	}
}
func TestTeamMessagesBoundConcurrentFanout(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org, ids := coordinatedFixture(t, s)
	f := lease(t, s, ids["lead"])
	if _, err := s.CreateProbe(ctx, org, f.SeatID); err != nil {
		t.Fatal(err)
	}
	d, err := s.LeaseNext(ctx, f)
	if err != nil || d == nil {
		t.Fatal("lease", err)
	}
	msg := SeatMessage{OrganizationID: org, SenderID: f.SeatID, RecipientID: ids["engineer"], ExecutionID: d.ExecutionID, Body: "peer request", Route: "peer"}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			local := msg
			if i%2 == 0 {
				local.ExecutionID = strings.ToUpper(msg.ExecutionID)
			}
			_, err := s.SendSeatMessage(ctx, f, local)
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("send: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 3 {
		t.Fatalf("concurrent fanout accepted %d want3", wins.Load())
	}
	msg.ExecutionID = ""
	if _, err := s.SendSeatMessage(ctx, f, msg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing execution bypass: %v", err)
	}
}

func TestUncappedMessagesCannotAttributeAnotherExecution(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	org, ids := coordinatedFixture(t, s)
	victim := lease(t, s, ids["lead"])
	if _, err := s.CreateProbe(ctx, org, victim.SeatID); err != nil {
		t.Fatal(err)
	}
	d, err := s.LeaseNext(ctx, victim)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	uncapped := lease(t, s, ids["rep_b"])
	msg := SeatMessage{OrganizationID: org, SenderID: uncapped.SeatID, RecipientID: ids["engineer"], ExecutionID: d.ExecutionID, Body: "attempted attribution", Route: "direct"}
	if _, err := s.SendSeatMessage(ctx, uncapped, msg); !errors.Is(err, ErrFenced) {
		t.Fatalf("foreign seat execution accepted: %v", err)
	}
	// A second organization is independently scoped even with the same seat keys.
	m := orgfixture.Manifest(t)
	other, err := s.SyncOrganization(ctx, SyncInput{Namespace: "other", Key: "acme", SourceUID: "other", Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	foreign := lease(t, s, other.Seats["lead"].SeatID)
	if _, err := s.CreateProbe(ctx, other.OrganizationID, foreign.SeatID); err != nil {
		t.Fatal(err)
	}
	fd, err := s.LeaseNext(ctx, foreign)
	if err != nil || fd == nil {
		t.Fatal(err)
	}
	msg.ExecutionID = fd.ExecutionID
	if _, err := s.SendSeatMessage(ctx, uncapped, msg); !errors.Is(err, ErrFenced) {
		t.Fatalf("foreign org execution accepted: %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE execution_id=$1 OR execution_id=$2`, d.ExecutionID, fd.ExecutionID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("quota polluted: %d %v", n, err)
	}
}

func TestOperatorMissionChangesFenceStaleRefinement(t *testing.T) {
	for _, withHistory := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "existing"}[withHistory], func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			_, ids := coordinatedFixture(t, s)
			f := lease(t, s, ids["lead"])
			old, err := s.TeamMission(ctx, f.SeatID, "engineering")
			if err != nil {
				t.Fatal(err)
			}
			if withHistory {
				old, err = s.UpdateTeamMission(ctx, f, "engineering", old.Revision, old.PolicyRevision, "runtime refinement", "progress")
				if err != nil {
					t.Fatal(err)
				}
			}
			sp := orgfixture.Spec()
			team := sp.Teams["engineering"]
			team.Coordination = &spec.TeamCoordination{Mission: "New operator goal", WorkStore: "engineering", MissionEditors: []string{"seat:lead"}}
			sp.Teams["engineering"] = team
			syncOrg(t, s, orgfixture.Compile(t, sp))
			if _, err = s.UpdateTeamMission(ctx, f, "engineering", old.Revision, old.PolicyRevision, "stale goal", "late client"); !errors.Is(err, ErrConflict) {
				t.Fatalf("operator policy bypass: %v", err)
			}
			current, err := s.TeamMission(ctx, f.SeatID, "engineering")
			if err != nil || current.Mission != "New operator goal" {
				t.Fatalf("mission: %+v %v", current, err)
			}
			if _, err = s.UpdateTeamMission(ctx, f, "engineering", current.Revision, current.PolicyRevision, "new compatible refinement", "seen new policy"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
