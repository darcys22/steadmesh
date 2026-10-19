//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativePoolAdmissionAndDurability(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	m := orgfixture.Compile(t, sp)
	o := syncOrg(t, s, m)
	f := lease(t, s, o.Seats["lead"].SeatID)
	members, err := s.ListPoolMembers(ctx, f.SeatID, "engineering")
	if err != nil || len(members) != 1 {
		t.Fatalf("minimum: %v %v", members, err)
	}
	var spent int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE seat_id=$1`, members[0].SeatID).Scan(&spent); err != nil || spent != 0 {
		t.Fatalf("min executed model work: %d %v", spent, err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.SpawnPoolMember(ctx, f, "engineering", "engineer", uuid.NewString())
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("admit: %v", err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 2 {
		t.Fatalf("admitted %d; expected exactly max-min", admitted.Load())
	}
	members, err = s.ListPoolMembers(ctx, f.SeatID, "engineering")
	if err != nil || len(members) != 3 {
		t.Fatalf("members: %v %v", members, err)
	}
	first := members[0]
	mf := lease(t, s, first.SeatID)
	if err = s.DrainPoolMember(ctx, f, "engineering", first.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SpawnPoolMember(ctx, f, "engineering", "engineer", uuid.NewString()); !errors.Is(err, ErrConflict) {
		t.Fatalf("draining member did not consume capacity: %v", err)
	}
	// Re-sync cannot resurrect a draining member or retire an unlisted live member.
	again := syncOrg(t, s, m)
	if _, ok := again.RuntimeSeats[members[1].SeatKey]; !ok {
		t.Fatal("live pool member missing from runtime overlay")
	}
	var retiring bool
	if err = s.pool.QueryRow(ctx, `SELECT retiring_at IS NOT NULL FROM seats WHERE id=$1`, first.SeatID).Scan(&retiring); err != nil || !retiring {
		t.Fatal("sync cancelled drain", err)
	}
	// Removing template revokes the principal and retains private memory rows.
	team := sp.Teams["engineering"]
	team.Pool = nil
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, err = s.AcquireLease(ctx, first.SeatID, "new-pod"); err == nil {
		t.Fatal("revoked participant acquired lease")
	}
	if err = s.inTx(ctx, func(tx pgx.Tx) error { return checkFence(ctx, tx, mf) }); err == nil {
		t.Fatal("revoked execution fence survived")
	}
	var stores int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM memory_stores WHERE owner_seat_id=$1`, first.SeatID).Scan(&stores); err != nil || stores != 1 {
		t.Fatalf("private memory lost: %d %v", stores, err)
	}
}

func TestNativePoolBudgetSurvivesReplacementAndHandoff(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	sp.ModelBudgets.OrganizationDailyOutputTokens = 10
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	lead := o.Seats["lead"].SeatID
	members, err := s.ListPoolMembers(ctx, lead, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	old := members[0]
	f := lease(t, s, old.SeatID)
	execution := uuid.NewString()
	_, err = s.pool.Exec(ctx, `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) VALUES($1,$2,$3,'r','fake')`, execution, f.SeatID, f.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveModelOutput(ctx, o.OrganizationID, f, execution, false, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveHandoff(ctx, f, runtimeapi.Handoff{Objective: "Keep this durable handoff", Notes: "Private recovery state"}); err != nil {
		t.Fatal(err)
	}
	team := sp.Teams["engineering"]
	team.Pool.MinMembers = 0
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp))
	lf := lease(t, s, lead)
	if err = s.DrainPoolMember(ctx, lf, "engineering", old.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRetirement(ctx, old.SeatID, nil); err != nil {
		t.Fatal(err)
	}
	h, err := s.Handoff(ctx, old.SeatID)
	if err != nil || h == nil || h.Objective != "Keep this durable handoff" {
		t.Fatalf("handoff lost: %v %v", h, err)
	}
	team = sp.Teams["engineering"]
	team.Pool.MinMembers = 1
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp)) // availability may replenish without spending
	members, err = s.ListPoolMembers(ctx, lead, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	var replacement string
	for _, p := range members {
		if p.State == "available" {
			replacement = p.SeatID
		}
	}
	if replacement == "" || replacement == old.SeatID {
		t.Fatal("missing independent replacement identity")
	}
	nf := lease(t, s, replacement)
	next := uuid.NewString()
	_, err = s.pool.Exec(ctx, `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) VALUES($1,$2,$3,'r','fake')`, next, nf.SeatID, nf.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReserveModelOutput(ctx, o.OrganizationID, nf, next, false, 10, "")
	var exhausted *BudgetExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("replacement reset shared budget: %v", err)
	}
}

func TestNativePoolIdleDrainRespectsMinimumAndBlocksChildren(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	m := orgfixture.Compile(t, sp)
	o := syncOrg(t, s, m)
	f := lease(t, s, o.Seats["lead"].SeatID)
	for range 2 {
		if _, err := s.SpawnPoolMember(ctx, f, "engineering", "engineer", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE native_pool_members SET created_at=now()-interval '2 hours' WHERE organization_id=$1`, o.OrganizationID); err != nil {
		t.Fatal(err)
	}
	syncOrg(t, s, m)
	members, err := s.ListPoolMembers(ctx, f.SeatID, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	available, draining := 0, 0
	var member string
	for _, p := range members {
		if p.State == "available" {
			available++
			member = p.SeatID
		}
		if p.State == "draining" {
			draining++
		}
	}
	if available != 1 || draining != 2 {
		t.Fatalf("idle pool available=%d draining=%d", available, draining)
	}
	if err = s.DrainPoolMember(ctx, f, "engineering", member); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual drain breached minimum: %v", err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		return workerAdmission(ctx, tx, o.OrganizationID, member, "", &sp, runtimeapi.TaskRunRequest{})
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("native member admitted legacy child: %v", err)
	}
}

func TestNativePoolPurgeClearsNewForeignKeys(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	team := sp.Teams["engineering"]
	team.Context = &spec.TeamContext{EditableSummary: true}
	team.Coordination.MissionEditors = []string{"seat:lead"}
	sp.Teams["engineering"] = team
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	lead := lease(t, s, o.Seats["lead"].SeatID)
	p, err := s.SpawnPoolMember(ctx, lead, "engineering", "engineer", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, p.SeatID)
	if _, err = s.UpdateParticipantContext(ctx, f, o.OrganizationID, "summary", "durable", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateTeamMission(ctx, lead, "engineering", 0, orgfixture.Compile(t, sp).Digest, "Ship safely", "test purge"); err != nil {
		t.Fatal(err)
	}
	execution := uuid.NewString()
	_, err = s.pool.Exec(ctx, `INSERT INTO executions(id,seat_id,lease_generation,config_revision,harness_adapter) VALUES($1,$2,$3,'r','fake')`, execution, f.SeatID, f.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendSeatMessage(ctx, f, SeatMessage{ExecutionID: execution, OrganizationID: o.OrganizationID, SenderID: f.SeatID, RecipientID: lead.SeatID, Body: "purge linked execution"}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOrganization(ctx, o.OrganizationID, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"organizations", "seats", "native_pool_members", "participant_context", "participant_context_history", "team_missions", "messages", "executions"} {
		var n int
		if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s remains: %d %v", table, n, err)
		}
	}
}

func TestNativePoolRevocationFinishesOwnedWork(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	members, err := s.ListPoolMembers(ctx, o.Seats["lead"].SeatID, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	member := members[0]
	f := lease(t, s, member.SeatID)
	ids, err := s.ActiveStoreIDs(ctx, o.OrganizationID, []string{"engineering"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateStructured(ctx, f, o.OrganizationID, ids["engineering"], f.SeatID, KindWork, "work", "W", func(n int64) (json.RawMessage, string, error) {
		raw, _ := json.Marshal(map[string]string{"id": "engineering/W-1", "owner": member.SeatKey, "status": "active", "objective": "Finish me"})
		return raw, "owned work", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveHandoff(ctx, f, runtimeapi.Handoff{Objective: "Unfinished objective", Notes: "Review retained evidence"}); err != nil {
		t.Fatal(err)
	}
	team := sp.Teams["engineering"]
	team.Pool = nil
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, err = s.Seat(ctx, f.SeatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked auth still live: %v", err)
	}
	if err = s.inTx(ctx, func(tx pgx.Tx) error {
		return checkFence(ctx, tx, Fence{SeatID: f.SeatID, Generation: f.Generation + 1})
	}); !errors.Is(err, ErrFenced) {
		t.Fatalf("guessed revoked generation accepted: %v", err)
	}
	guessed := Fence{SeatID: f.SeatID, Generation: f.Generation + 1}
	if err = s.ReportState(ctx, guessed, runtimeapi.StateRequest{State: "Warm"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("revocation overwritten by report: %v", err)
	}
	if err = s.ReleaseLease(ctx, f.SeatID, guessed.Generation); !errors.Is(err, ErrFenced) {
		t.Fatalf("revocation overwritten by release: %v", err)
	}
	if _, err = s.FenceSeat(ctx, o.OrganizationID, f.SeatID, guessed.Generation); !errors.Is(err, ErrConflict) {
		t.Fatalf("revocation overwritten by controller: %v", err)
	}
	due, err := s.DueRetirements(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		found = found || d.SeatID == f.SeatID
	}
	if !found {
		t.Fatal("revocation bypassed normal finalization")
	}
	result, err := s.FinishRetirement(ctx, f.SeatID, func(raw json.RawMessage) (json.RawMessage, string, error) {
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, "", err
		}
		data["owner"] = ""
		data["status"] = "ready"
		out, err := json.Marshal(data)
		return out, "released by retirement", err
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.ReleasedWork) != 1 || len(result.Notified) != 2 {
		t.Fatalf("incomplete finalization: %+v", result)
	}
	var owner, status string
	if err = s.pool.QueryRow(ctx, `SELECT data->>'owner',data->>'status' FROM memory_records WHERE id=$1`, rec.ID).Scan(&owner, &status); err != nil || owner != "" || status != "ready" {
		t.Fatalf("work stuck: %q %q %v", owner, status, err)
	}
	var retired bool
	if err = s.pool.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM memory_stores WHERE owner_seat_id=$1`, f.SeatID).Scan(&retired); err != nil || !retired {
		t.Fatalf("private store not finalized: %v", err)
	}
	h, err := s.Handoff(ctx, f.SeatID)
	if err != nil || h == nil || h.Objective != "Unfinished objective" {
		t.Fatalf("handoff lost: %v %v", h, err)
	}
}

func TestNativePoolOptInPrivateRetention(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	team := sp.Teams["engineering"]
	team.Pool.RetentionSeconds = 600
	team.Context = &spec.TeamContext{EditableSummary: true}
	sp.Teams["engineering"] = team
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	lead := lease(t, s, o.Seats["lead"].SeatID)
	members, err := s.ListPoolMembers(ctx, lead.SeatID, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	member := members[0]
	f := lease(t, s, member.SeatID)
	seat, err := s.Seat(ctx, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	stores, err := s.ActiveStoreIDs(ctx, o.OrganizationID, []string{seat.Manifest.PersonalMemory, "engineering"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{seat.Manifest.PersonalMemory, "engineering"} {
		if _, _, err = s.WriteFile(ctx, f, FileWrite{OrganizationID: o.OrganizationID, StoreID: stores[key], AuthorID: f.SeatID, Path: "notes/evidence", Text: "retain based on scope"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SaveHandoff(ctx, f, runtimeapi.Handoff{Objective: "private handoff"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateParticipantContext(ctx, f, o.OrganizationID, "summary", "private summary", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCheckpoint(ctx, f, runtimeapi.Checkpoint{HarnessAdapter: "fake", FormatVersion: "v1", CheckpointRef: "private-ref", Guarantee: "application_checkpoint"}); err != nil {
		t.Fatal(err)
	}
	var privateRecord string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM memory_records WHERE store_id=$1`, stores[seat.Manifest.PersonalMemory]).Scan(&privateRecord); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO artifacts(id,organization_id,store_id,owner_seat_id,location,content_digest,size_bytes) VALUES($1,$2,$3,$4,'retained-evidence','sha256:test',1)`, uuid.NewString(), o.OrganizationID, stores[seat.Manifest.PersonalMemory], f.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO work_publications(organization_id,record_id,connection,external_kind) VALUES($1,$2,'tracker','task')`, o.OrganizationID, privateRecord); err != nil {
		t.Fatal(err)
	}
	countPrivate := func() int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM memory_records WHERE store_id=$1`, stores[seat.Manifest.PersonalMemory]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	_, err = s.pool.Exec(ctx, `UPDATE native_pool_members SET retained_until=now()-interval '1 hour' WHERE seat_id=$1`, f.SeatID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PurgeExpiredPoolState(ctx, 100); err != nil || countPrivate() != 1 {
		t.Fatal("live private data purged", err)
	}
	team = sp.Teams["engineering"]
	team.Pool.MinMembers = 0
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if err = s.DrainPoolMember(ctx, lead, "engineering", f.SeatID); err != nil {
		t.Fatal(err)
	}
	if err = s.PurgeExpiredPoolState(ctx, 100); err != nil || countPrivate() != 1 {
		t.Fatal("draining private data purged", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE native_pool_members SET retained_until=NULL WHERE seat_id=$1`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRetirement(ctx, f.SeatID, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.PurgeExpiredPoolState(ctx, 100); err != nil || countPrivate() != 1 {
		t.Fatal("retention horizon not honored", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE native_pool_members SET retained_until=now()-interval '1 second' WHERE seat_id=$1`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if err = s.PurgeExpiredPoolState(ctx, 100); err != nil {
		t.Fatal(err)
	} // the retirement loop invokes this independently of finalization
	if countPrivate() != 0 {
		t.Fatal("expired private records retained")
	}
	for _, table := range []string{"participant_context", "participant_context_history", "sessions", "handoffs"} {
		var n int
		if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE seat_id=$1", f.SeatID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("expired %s retained: %d %v", table, n, err)
		}
	}
	var shared int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM memory_records WHERE store_id=$1`, stores["engineering"]).Scan(&shared); err != nil || shared != 1 {
		t.Fatal("shared evidence deleted", err)
	}
	var detached bool
	if err = s.pool.QueryRow(ctx, `SELECT store_id IS NULL FROM artifacts WHERE owner_seat_id=$1`, f.SeatID).Scan(&detached); err != nil || !detached {
		t.Fatalf("retained artifact not detached: %v", err)
	}
	var purged bool
	if err = s.pool.QueryRow(ctx, `SELECT purged_at IS NOT NULL FROM native_pool_members WHERE seat_id=$1`, f.SeatID).Scan(&purged); err != nil || !purged {
		t.Fatal("purge audit missing", err)
	}
}

func TestNativePoolZeroRetentionPreservesPrivateState(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sp := nativePoolSpec()
	team := sp.Teams["engineering"]
	team.Pool.RetentionSeconds = 0
	sp.Teams["engineering"] = team
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	members, err := s.ListPoolMembers(ctx, o.Seats["lead"].SeatID, "engineering")
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, members[0].SeatID)
	if err = s.SaveHandoff(ctx, f, runtimeapi.Handoff{Objective: "Retain indefinitely"}); err != nil {
		t.Fatal(err)
	}
	team.Pool = nil
	sp.Teams["engineering"] = team
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, err = s.FinishRetirement(ctx, f.SeatID, nil); err != nil {
		t.Fatal(err)
	}
	var seconds int
	var until *time.Time
	if err = s.pool.QueryRow(ctx, `SELECT retention_seconds,retained_until FROM native_pool_members WHERE seat_id=$1`, f.SeatID).Scan(&seconds, &until); err != nil || seconds != 0 || until != nil {
		t.Fatalf("zero retention scheduled expiry: %d %v %v", seconds, until, err)
	}
	if err = s.PurgeExpiredPoolState(ctx, 100); err != nil {
		t.Fatal(err)
	}
	h, err := s.Handoff(ctx, f.SeatID)
	if err != nil || h == nil || h.Objective != "Retain indefinitely" {
		t.Fatalf("zero retention deleted state: %v %v", h, err)
	}
}
