//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func changeTestSkill(s *Store, ctx context.Context, f Fence, orgID string, ref spec.SkillRef, source spec.SkillMarketplace, install bool) error {
	snapshot, err := s.SkillsSnapshot(ctx, orgID, f.SeatID)
	if err != nil {
		return err
	}
	return s.ChangeSkillSelection(ctx, f, orgID, ref, source, install, snapshot.Fingerprint())
}

func TestSkillChoicesPersistAndRevocationCannotResurrect(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	policy := skillTestManifest("agent_choice")
	sm.Skills = policy.Skills
	sm.SkillMarketplaces = policy.SkillMarketplaces
	m.Seats["lead"] = sm
	res := syncOrg(t, s, m)
	id := res.Seats["lead"].SeatID
	f := lease(t, s, id)
	source := sm.SkillMarketplaces["allowed"]
	ref := skillTestRef("review")
	if err := changeTestSkill(s, ctx, f, res.OrganizationID, ref, source, true); err != nil {
		t.Fatal(err)
	}
	again := syncOrg(t, s, m)
	if again.Seats["lead"].PolicyRevision != res.Seats["lead"].PolicyRevision {
		t.Fatal("unchanged apply changed authority")
	}
	got, err := s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 1 || got.Selections[0].Owner != "agent" {
		t.Fatalf("apply lost runtime choice: %+v %v", got, err)
	}
	if len(m.Seats["lead"].Skills.Install) != 0 {
		t.Fatal("runtime choice leaked into declaration")
	}
	other, err := s.SkillsSnapshot(ctx, res.OrganizationID, res.Seats["reviewer"].SeatID)
	if err != nil || len(other.Selections) != 0 {
		t.Fatalf("another seat inherited choices: %+v %v", other, err)
	}
	if _, err := s.SkillsSnapshot(ctx, "00000000-0000-0000-0000-000000000000", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-organization access: %v", err)
	}
	changed := source
	changed.Ref = strings.Repeat("b", 40)
	sm.SkillMarketplaces["allowed"] = changed
	m.Seats["lead"] = sm
	revoked := syncOrg(t, s, m)
	if revoked.Seats["lead"].PolicyRevision <= again.Seats["lead"].PolicyRevision {
		t.Fatal("source change did not bump authority")
	}
	if err := changeTestSkill(s, ctx, f, res.OrganizationID, ref, source, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale resolved source: %v", err)
	}
	sm.SkillMarketplaces["allowed"] = source
	m.Seats["lead"] = sm
	syncOrg(t, s, m)
	got, err = s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 0 {
		t.Fatalf("re-added source resurrected revoked choices: %+v %v", got, err)
	}
	if err := changeTestSkill(s, ctx, f, res.OrganizationID, ref, source, true); err != nil {
		t.Fatal(err)
	}
	sm.Skills = nil
	m.Seats["lead"] = sm
	syncOrg(t, s, m)
	got, err = s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 0 {
		t.Fatalf("disabled policy retained skills: %+v %v", got, err)
	}
	if err := changeTestSkill(s, ctx, f, res.OrganizationID, ref, source, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disabled install: %v", err)
	}
}

func TestSkillChoicesOwnershipFenceAndConcurrentLimit(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	policy := skillTestManifest("agent_choice")
	sm.Skills = policy.Skills
	sm.SkillMarketplaces = policy.SkillMarketplaces
	static := skillTestRef("static")
	sm.Skills.Install = []spec.SkillRef{static}
	m.Seats["lead"] = sm
	res := syncOrg(t, s, m)
	id := res.Seats["lead"].SeatID
	f := lease(t, s, id)
	source := sm.SkillMarketplaces["allowed"]
	if err := changeTestSkill(s, ctx, f, res.OrganizationID, static, source, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("removed Terraform skill: %v", err)
	}
	if err := changeTestSkill(s, ctx, Fence{SeatID: id, Generation: f.Generation + 1}, res.OrganizationID, skillTestRef("stale"), source, true); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale fence: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			results <- changeTestSkill(s, ctx, f, res.OrganizationID, skillTestRef(name), source, true)
		}(name)
	}
	wg.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || limited != 1 {
		t.Fatalf("concurrent limit winners=%d denied=%d", success, limited)
	}
	got, err := s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 2 {
		t.Fatalf("static must not consume dynamic capacity: %+v %v", got, err)
	}
	for _, v := range got.Selections {
		if v.Owner == "agent" {
			if err := changeTestSkill(s, ctx, f, res.OrganizationID, v.Ref, source, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err = s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 1 || got.Selections[0].Owner != "terraform" {
		t.Fatalf("remove changed static: %+v %v", got, err)
	}
}

func TestSkillSelectionRechecksManifestInsideWriteTransaction(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	policy := skillTestManifest("agent_choice")
	sm.Skills = policy.Skills
	sm.SkillMarketplaces = policy.SkillMarketplaces
	m.Seats["lead"] = sm
	res := syncOrg(t, s, m)
	id := res.Seats["lead"].SeatID
	f := lease(t, s, id)
	source := sm.SkillMarketplaces["allowed"]
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, res.OrganizationID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- changeTestSkill(s, ctx, f, res.OrganizationID, skillTestRef("late"), source, true) }()
	select {
	case err := <-done:
		t.Fatalf("mutation bypassed organization policy lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	sm.Skills = &spec.SkillPolicy{Mode: "discover", Marketplaces: []string{"allowed"}}
	raw, _ := json.Marshal(sm)
	if _, err := tx.Exec(ctx, `UPDATE seats SET manifest=$2 WHERE id=$1`, id, raw); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("committed stale authorization: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation failed to unblock")
	}
	got, err := s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil || len(got.Selections) != 0 {
		t.Fatalf("stale mutation persisted: %+v %v", got, err)
	}
}

func TestSkillSelectionRejectsStaleValidatedUnion(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["lead"]
	policy := skillTestManifest("agent_choice")
	policy.Skills.MaxDynamicSkills = 16
	sm.Skills = policy.Skills
	sm.SkillMarketplaces = policy.SkillMarketplaces
	m.Seats["lead"] = sm
	res := syncOrg(t, s, m)
	id := res.Seats["lead"].SeatID
	f := lease(t, s, id)
	source := sm.SkillMarketplaces["allowed"]
	snapshot, err := s.SkillsSnapshot(ctx, res.OrganizationID, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeSkillSelection(ctx, f, res.OrganizationID, skillTestRef("first"), source, true, snapshot.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeSkillSelection(ctx, f, res.OrganizationID, skillTestRef("second"), source, true, snapshot.Fingerprint()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale aggregate validation accepted: %v", err)
	}
	if err := s.ChangeSkillSelection(ctx, f, res.OrganizationID, skillTestRef("second"), source, true, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing validation token accepted: %v", err)
	}
}
