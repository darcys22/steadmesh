//go:build integration

package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func TestParticipantContextRevisionIsolationAndRollback(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	sm.Context = &spec.TeamContext{EditableInstructions: true, EditableSummary: true}
	m.Seats["lead"] = sm
	res := syncOrg(t, s, m)
	id := res.Seats["lead"].SeatID
	other := res.Seats["reviewer"].SeatID
	f := lease(t, s, id)
	initial, err := s.ParticipantContext(ctx, res.OrganizationID, id, 0)
	if err != nil || initial.Revision != 0 {
		t.Fatalf("initial: %+v %v", initial, err)
	}
	v1, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "instructions", "local plan", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Revision != 1 || v1.ActorSeatID != id {
		t.Fatalf("attribution: %+v", v1)
	}
	// Concurrent revision checks serialize, allowing exactly one winner.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "summary", "new summary", 1, 0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS outcomes %d %d", success, conflict)
	}
	v3, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "summary", "", 2, 1)
	if err != nil || v3.Revision != 3 || v3.Summary != "" || v3.Instructions != "local plan" {
		t.Fatalf("rollback: %+v %v", v3, err)
	}
	history, err := s.ParticipantContextHistory(ctx, res.OrganizationID, id, 0, 20)
	if err != nil || len(history) != 3 {
		t.Fatalf("history: %+v %v", history, err)
	}
	old, err := s.ParticipantContext(ctx, res.OrganizationID, id, 1)
	if err != nil || old != v1 {
		t.Fatalf("old changed: %+v %v", old, err)
	}
	if _, err := s.ParticipantContext(ctx, res.OrganizationID, other, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other inherited history: %v", err)
	}
	if _, err := s.ParticipantContext(ctx, "00000000-0000-0000-0000-000000000000", id, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org read: %v", err)
	}
	if _, err := s.UpdateParticipantContext(ctx, Fence{SeatID: id, Generation: f.Generation + 1}, res.OrganizationID, "summary", "stale", 3, 0); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale lease: %v", err)
	}
	// Revoking prompt editing does not revoke summary editing.
	sm.Context = &spec.TeamContext{EditableSummary: true}
	m.Seats["lead"] = sm
	syncOrg(t, s, m)
	if _, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "instructions", "overwrite operator", 3, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prompt permission: %v", err)
	}
	if _, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "summary", "still permitted", 3, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateParticipantContext(ctx, f, res.OrganizationID, "mission", "new mission", 4, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operator envelope: %v", err)
	}
}
