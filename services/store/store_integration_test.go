//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func syncOrg(t *testing.T, s *Store, m *compile.Manifest) *runtimeapi.SyncResponse {
	t.Helper()
	res, err := s.SyncOrganization(context.Background(), SyncInput{Namespace: "acme", Key: "acme", SourceUID: "uid-1", Manifest: m})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

// lease acquires the seat's lease and returns its fence.
func lease(t *testing.T, s *Store, seatID string) Fence {
	t.Helper()
	l, err := s.AcquireLease(context.Background(), seatID, "pod-"+seatID)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return Fence{SeatID: seatID, Generation: l.Generation}
}

func storeIDs(t *testing.T, s *Store, org string, keys ...string) []string {
	t.Helper()
	m, err := s.ActiveStoreIDs(context.Background(), org, keys)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, k := range keys {
		if id, ok := m[k]; ok {
			out = append(out, id)
		}
	}
	return out
}

func TestSyncIsIdempotentAndBumpsPolicyOnAuthorityChange(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := syncOrg(t, s, orgfixture.Manifest(t))
	again := syncOrg(t, s, orgfixture.Manifest(t))
	if first.OrganizationID != again.OrganizationID {
		t.Fatal("organisation id changed")
	}
	for k, id := range first.Seats {
		if again.Seats[k] != id {
			t.Fatalf("seat %s identity changed: %+v -> %+v", k, id, again.Seats[k])
		}
	}

	sp := orgfixture.Spec()
	delete(sp.Grants, "lead_tracker")
	third := syncOrg(t, s, orgfixture.Compile(t, sp))
	if got := third.Seats["lead"].PolicyRevision; got != 2 {
		t.Fatalf("lead policy revision = %d, want 2", got)
	}
	if got := third.Seats["engineer"].PolicyRevision; got != 1 {
		t.Fatalf("engineer policy revision = %d, want 1", got)
	}
	seat, err := s.Seat(ctx, third.Seats["lead"].SeatID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range seat.Manifest.Capabilities {
		if c.Resource == "connection:tracker" {
			t.Fatal("revoked grant still in committed manifest")
		}
	}
}

func TestRetireAndRecreateDoesNotInheritPrivateData(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, old := res.OrganizationID, res.Seats["rep_a"].SeatID
	f := lease(t, s, old)
	rec, _, err := s.WriteFile(ctx, f, FileWrite{OrganizationID: org, StoreID: storeIDs(t, s, org, "rep_a")[0], AuthorID: old,
		Path: "notes/alice-prefers-terse-updates.md", Text: "private preference"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestHuman(ctx, HumanMessage{OrganizationID: org, Connection: "slack", Binding: "alice",
		ExternalUserID: orgfixture.UserA, ExternalRef: "D1", EventID: "ev1", RepresentativeID: old, Body: "secret"}); err != nil {
		t.Fatal(err)
	}

	sp := orgfixture.Spec()
	delete(sp.Seats, "rep_a")
	delete(sp.ChannelBindings, "alice")
	delete(sp.Grants, "org_memory_read")
	delete(sp.MessageRoutes, "rep_a_to_lead")
	res = syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, ok := res.Retiring["rep_a"]; !ok || len(res.Retiring) != 1 {
		t.Fatalf("retiring = %v", res.Retiring)
	}
	// With no grace period the seat is due at once.
	due, err := s.DueRetirements(ctx, 10)
	if err != nil || len(due) != 1 || due[0].SeatID != old || !due[0].Overdue {
		t.Fatalf("due = %+v %v", due, err)
	}
	if r, err := s.FinishRetirement(ctx, old, nil); err != nil || r == nil {
		t.Fatalf("finish retirement: %+v %v", r, err)
	}
	if r, err := s.FinishRetirement(ctx, old, nil); err != nil || r != nil {
		t.Fatalf("second finish: %+v %v", r, err)
	}
	if _, err := s.Seat(ctx, old); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired seat still active: %v", err)
	}
	if err := s.Ack(ctx, f, 1, runtimeapi.InboxAckRequest{}); !errors.Is(err, ErrFenced) {
		t.Fatalf("retired seat's lease still valid: %v", err)
	}

	res = syncOrg(t, s, orgfixture.Manifest(t))
	fresh := res.Seats["rep_a"].SeatID
	if fresh == old {
		t.Fatal("recreated seat reused the retired identity")
	}
	ids := storeIDs(t, s, org, "rep_a")
	if _, err := s.Record(ctx, org, rec.ID, ids); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new identity can read retired private record: %v", err)
	}
	hits, err := s.Search(ctx, org, ids, "", "", nil, 0, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("new identity sees retired records: %v %v", hits, err)
	}
	convs, err := s.Conversations(ctx, org, fresh, 0, 10)
	if err != nil || len(convs) != 0 {
		t.Fatalf("new identity sees retired conversations: %v %v", convs, err)
	}
	// The next human message starts a fresh conversation for the new identity.
	if _, err := s.IngestHuman(ctx, HumanMessage{OrganizationID: org, Connection: "slack", Binding: "alice",
		ExternalUserID: orgfixture.UserA, ExternalRef: "D1", EventID: "ev2", RepresentativeID: fresh, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	convs, _ = s.Conversations(ctx, org, fresh, 0, 10)
	if len(convs) != 1 {
		t.Fatalf("conversations = %v", convs)
	}
	msgs, _ := s.ConversationMessages(ctx, org, convs[0].ID, 0, 10)
	if len(msgs) != 1 || msgs[0].Body != "hi" {
		t.Fatalf("fresh conversation leaked history: %+v", msgs)
	}
}

func TestAdoptionTransfersPersonalData(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, old := res.OrganizationID, res.Seats["lead"].SeatID
	rec, _, err := s.WriteFile(ctx, lease(t, s, old), FileWrite{OrganizationID: org, StoreID: storeIDs(t, s, org, "lead")[0],
		AuthorID: old, Path: "notes/plan.md", Text: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	sp := orgfixture.Spec()
	delete(sp.Seats, "lead")
	delete(sp.Grants, "lead_tracker")
	delete(sp.Grants, "org_memory_lead")
	delete(sp.MessageRoutes, "rep_a_to_lead")
	delete(sp.MessageRoutes, "rep_b_to_lead")
	delete(sp.MessageRoutes, "lead_engineer")
	syncOrg(t, s, orgfixture.Compile(t, sp))
	// A retiring seat cannot be adopted yet.
	sp2 := orgfixture.Spec()
	early := sp2.Seats["lead"]
	early.AdoptFrom = old
	sp2.Seats["lead"] = early
	if _, err := s.SyncOrganization(ctx, SyncInput{Namespace: "acme", Key: "acme", Manifest: orgfixture.Compile(t, sp2)}); err != nil {
		// Declaring the key again cancels the retirement instead.
		t.Fatalf("redeclare: %v", err)
	}
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, err := s.FinishRetirement(ctx, old, nil); err != nil {
		t.Fatal(err)
	}

	sp = orgfixture.Spec()
	lead := sp.Seats["lead"]
	lead.AdoptFrom = "00000000-0000-0000-0000-000000000000"
	sp.Seats["lead"] = lead
	if _, err := s.SyncOrganization(ctx, SyncInput{Namespace: "acme", Key: "acme", Manifest: orgfixture.Compile(t, sp)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("adopting unknown seat: %v", err)
	}
	lead.AdoptFrom = old
	sp.Seats["lead"] = lead
	res = syncOrg(t, s, orgfixture.Compile(t, sp))
	got, err := s.Record(ctx, org, rec.ID, storeIDs(t, s, org, "lead"))
	if err != nil || got.Text != "the plan" {
		t.Fatalf("adopted seat cannot read its retained record: %v", err)
	}
	if res.Seats["lead"].SeatID == old {
		t.Fatal("adoption must create a new identity")
	}
}

func TestLeaseFencing(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	seat := res.Seats["engineer"].SeatID
	l1, err := s.AcquireLease(ctx, seat, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, seat, "pod-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second pod acquired a held lease: %v", err)
	}
	if _, err := s.FenceSeat(ctx, res.OrganizationID, seat, l1.Generation+5); !errors.Is(err, ErrConflict) {
		t.Fatalf("fence with wrong generation: %v", err)
	}
	if _, err := s.FenceSeat(ctx, res.OrganizationID, seat, l1.Generation); err != nil {
		t.Fatal(err)
	}
	stale := Fence{SeatID: seat, Generation: l1.Generation}
	if _, _, err := s.WriteFile(ctx, stale, FileWrite{OrganizationID: res.OrganizationID,
		StoreID: storeIDs(t, s, res.OrganizationID, "engineer")[0], AuthorID: seat, Path: "notes/x.md", Text: "y"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale generation write: %v", err)
	}
	if _, err := s.RenewLease(ctx, seat, "pod-1", l1.Generation); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale renew: %v", err)
	}
	l2, err := s.AcquireLease(ctx, seat, "pod-2")
	if err != nil || l2.Generation <= l1.Generation {
		t.Fatalf("acquire after fence: %+v %v", l2, err)
	}
}

func TestRevisionConflict(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org := res.OrganizationID
	lead, eng := res.Seats["lead"].SeatID, res.Seats["engineer"].SeatID
	ids := storeIDs(t, s, org, "engineering")
	fl, fe := lease(t, s, lead), lease(t, s, eng)
	rec, _, err := s.WriteFile(ctx, fl, FileWrite{OrganizationID: org, StoreID: ids[0], AuthorID: lead, Path: "notes/design.md", Text: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	if _, _, err := s.WriteFile(ctx, fl, FileWrite{OrganizationID: org, StoreID: ids[0], AuthorID: lead, Path: rec.Path, Text: "v2 by lead", ExpectedRevision: &one}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.WriteFile(ctx, fe, FileWrite{OrganizationID: org, StoreID: ids[0], AuthorID: eng, Path: rec.Path, Text: "v2 by engineer", ExpectedRevision: &one})
	c, ok := IsConflict(err)
	if !ok || c.Current != 2 || c.CurrentAuthor != "lead" {
		t.Fatalf("expected conflict at revision 2 by lead, got %v", err)
	}
	hist, err := s.History(ctx, org, rec.ID, ids, 0, 10)
	if err != nil || len(hist) != 2 || hist[0].Author != "lead" || *hist[0].Generation != fl.Generation {
		t.Fatalf("history = %+v %v", hist, err)
	}
}

func TestSearchOnlyTouchesAuthorisedStores(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org := res.OrganizationID
	a, b := res.Seats["rep_a"].SeatID, res.Seats["rep_b"].SeatID
	if _, _, err := s.WriteFile(ctx, lease(t, s, b), FileWrite{OrganizationID: org, StoreID: storeIDs(t, s, org, "rep_b")[0],
		AuthorID: b, Path: "notes/acquisition-codename-falcon.md", Text: "falcon is confidential"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.WriteFile(ctx, lease(t, s, a), FileWrite{OrganizationID: org, StoreID: storeIDs(t, s, org, "rep_a")[0],
		AuthorID: a, Path: "notes/falcon-notes.md", Text: "public falcon notes", Tags: []string{"birds"}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, org, storeIDs(t, s, org, "rep_a", "organisation"), "falcon", "", nil, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Store != "rep_a" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits, _ := s.Search(ctx, org, storeIDs(t, s, org, "rep_a"), "falcon", "", []string{"birds"}, 0, 10); len(hits) != 1 {
		t.Fatalf("tag filter hits = %+v", hits)
	}
}

func TestIngestDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	in := HumanMessage{OrganizationID: res.OrganizationID, Connection: "slack", Binding: "alice", ExternalUserID: orgfixture.UserA,
		ExternalRef: "D1", EventID: "Ev42", RepresentativeID: res.Seats["rep_a"].SeatID, Body: "hello"}
	first, err := s.IngestHuman(ctx, in)
	if err != nil || first.Duplicate {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := s.IngestHuman(ctx, in)
	if err != nil || !again.Duplicate || again.MessageID != first.MessageID {
		t.Fatalf("%+v %v", again, err)
	}
	if n, _ := s.PendingDeliveries(ctx, in.RepresentativeID); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
}

func TestInboxOrderingAndPoisonMessage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org := res.OrganizationID
	lead, eng := res.Seats["lead"].SeatID, res.Seats["engineer"].SeatID
	fl := lease(t, s, lead)
	first, err := s.SendSeatMessage(ctx, fl, SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng, Body: "poison"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendSeatMessage(ctx, fl, SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng,
		ConversationID: first.ConversationID, Body: "second in same conversation"}); err != nil {
		t.Fatal(err)
	}
	other, err := s.SendSeatMessage(ctx, fl, SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng, Body: "unrelated"})
	if err != nil {
		t.Fatal(err)
	}

	fe := lease(t, s, eng)
	d1, err := s.LeaseNext(ctx, fe)
	if err != nil || d1 == nil || d1.Message.Body != "poison" || d1.Attempt != 1 {
		t.Fatalf("first lease: %+v %v", d1, err)
	}
	if d1.Message.SenderSeat != "lead" || d1.Message.ReplyRoute != "seat:lead" {
		t.Fatalf("envelope = %+v", d1.Message)
	}
	// The conversation with a leased delivery is skipped; the unrelated one is next.
	d2, err := s.LeaseNext(ctx, fe)
	if err != nil || d2 == nil || d2.Message.ConversationID != other.ConversationID {
		t.Fatalf("second lease: %+v %v", d2, err)
	}
	if d, _ := s.LeaseNext(ctx, fe); d != nil {
		t.Fatalf("expected nothing eligible, got %q", d.Message.Body)
	}
	var cfg string
	if err := s.pool.QueryRow(ctx, `SELECT config_revision FROM executions WHERE id = $1 AND lease_generation = $2`,
		d1.ExecutionID, fe.Generation).Scan(&cfg); err != nil || cfg == "" {
		t.Fatalf("execution does not record config revision: %q %v", cfg, err)
	}

	// The poison message fails five times and is dead-lettered without blocking the seat.
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		var d *runtimeapi.InboxDelivery
		if attempt == 1 {
			d = d1
		} else {
			if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET next_attempt_at = now() WHERE id = $1`, d1.DeliveryID); err != nil {
				t.Fatal(err)
			}
			if d, err = s.LeaseNext(ctx, fe); err != nil || d == nil || d.DeliveryID != d1.DeliveryID || d.Attempt != attempt {
				t.Fatalf("attempt %d: %+v %v", attempt, d, err)
			}
		}
		if err := s.Ack(ctx, fe, d.DeliveryID, runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID, Outcome: "failed", Error: "crash"}); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, d1.DeliveryID).Scan(&state); err != nil || state != "dead" {
		t.Fatalf("poison delivery state = %q %v", state, err)
	}
	next, err := s.LeaseNext(ctx, fe)
	if err != nil || next == nil || next.Message.Body != "second in same conversation" {
		t.Fatalf("seat blocked after poison message: %+v %v", next, err)
	}
	rt, err := s.SeatRuntimes(ctx, org, []string{"engineer"})
	if err != nil || rt["engineer"].DeadDeliveries != 1 {
		t.Fatalf("runtime = %+v %v", rt, err)
	}
}

func TestExpiredLeaseRequeuesWithBackoff(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, lead, eng := res.OrganizationID, res.Seats["lead"].SeatID, res.Seats["engineer"].SeatID
	if _, err := s.SendSeatMessage(ctx, lease(t, s, lead), SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	fe := lease(t, s, eng)
	d, err := s.LeaseNext(ctx, fe)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET leased_until = now() - interval '1 second' WHERE id = $1`, d.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequeueExpired(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var wait float64
	if err := s.pool.QueryRow(ctx, `SELECT state, EXTRACT(EPOCH FROM next_attempt_at - now())::float8 FROM deliveries WHERE id = $1`,
		d.DeliveryID).Scan(&state, &wait); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || wait <= 0 || wait > 1.5 {
		t.Fatalf("state=%s wait=%.2fs", state, wait)
	}
	// A new lease generation requeues deliveries held by the old one.
	if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET next_attempt_at = now() WHERE id = $1`, d.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.LeaseNext(ctx, fe); d == nil {
		t.Fatal("not re-leased")
	}
	if _, err := s.FenceSeat(ctx, org, eng, fe.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, d.DeliveryID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("state after fence = %s %v", state, err)
	}
}

func TestInboxCap(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, lead, eng, rep := res.OrganizationID, res.Seats["lead"].SeatID, res.Seats["engineer"].SeatID, res.Seats["rep_a"].SeatID
	for _, seat := range []string{eng, rep} {
		if _, err := s.pool.Exec(ctx, `WITH c AS (INSERT INTO conversations (id, organization_id, kind) VALUES (gen_random_uuid(), $1, 'system') RETURNING id),
			m AS (INSERT INTO messages (id, organization_id, conversation_id, origin, recipient_seat_id, body)
				SELECT gen_random_uuid(), $1, c.id, 'system', $2, 'filler' FROM c, generate_series(1, $3) RETURNING id)
			INSERT INTO deliveries (message_id, seat_id) SELECT id, $2 FROM m`, org, seat, MaxPendingDeliveries); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SendSeatMessage(ctx, lease(t, s, lead), SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng, Body: "x"}); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("send to full inbox: %v", err)
	}
	got, err := s.IngestHuman(ctx, HumanMessage{OrganizationID: org, Connection: "slack", Binding: "alice", ExternalUserID: orgfixture.UserA,
		ExternalRef: "D1", EventID: "e", RepresentativeID: rep, Body: "still accepted"})
	if err != nil || !got.OverCap {
		t.Fatalf("human ingress over cap: %+v %v", got, err)
	}
	if n, _ := s.PendingDeliveries(ctx, rep); n != MaxPendingDeliveries+1 {
		t.Fatalf("human message dropped: pending=%d", n)
	}
}

func TestScheduleFiresOncePerTrigger(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, eng := res.OrganizationID, res.Seats["engineer"].SeatID
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	sc, err := s.CreateSchedule(ctx, lease(t, s, eng), org, "@every 1h", at, "standup")
	if err != nil {
		t.Fatal(err)
	}
	next := func(_ string, fired, _ time.Time) (time.Time, bool) { return fired.Add(time.Hour), true }
	if n, err := s.FireDue(ctx, time.Now(), 10, next); err != nil || n != 1 {
		t.Fatalf("fired %d %v", n, err)
	}
	// Simulate a replay of the same trigger time.
	if _, err := s.pool.Exec(ctx, `UPDATE wake_schedules SET next_trigger_at = $2 WHERE id = $1`, sc.ID, at); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FireDue(ctx, time.Now(), 10, next); err != nil || n != 0 {
		t.Fatalf("replayed fire queued %d wakes (%v)", n, err)
	}
	if n, _ := s.PendingDeliveries(ctx, eng); n != 1 {
		t.Fatalf("pending = %d", n)
	}
}

func TestOutboxClaimAndUnknownOnInterruptedSend(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, rep := res.OrganizationID, res.Seats["rep_a"].SeatID
	sent, err := s.ReplyToHuman(ctx, lease(t, s, rep), HumanReply{OrganizationID: org, SeatID: rep, Binding: "alice", Connection: "slack", Body: "update"})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimOutbox(ctx, 10)
	if err != nil || len(items) != 1 || items[0].MessageID != sent.MessageID || items[0].Binding != "alice" {
		t.Fatalf("claim: %+v %v", items, err)
	}
	if again, _ := s.ClaimOutbox(ctx, 10); len(again) != 0 {
		t.Fatal("claimed row claimed twice")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireSending(ctx); err != nil || n != 1 {
		t.Fatalf("expire sending: %d %v", n, err)
	}
	op, err := s.Operation(ctx, org, rep, items[0].OperationID)
	if err != nil || op.Status != OpUnknown {
		t.Fatalf("op = %+v %v", op, err)
	}
	if unknown, _ := s.UnknownOperations(ctx, rep, 10); len(unknown) != 1 {
		t.Fatalf("unknown ops = %v", unknown)
	}
}

func TestDeleteOrganization(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	if err := s.DeleteOrganization(ctx, res.OrganizationID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seat(ctx, res.Seats["lead"].SeatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seat active after retain-delete: %v", err)
	}
	var seats int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM seats WHERE organization_id = $1`, res.OrganizationID).Scan(&seats)
	if seats != 5 {
		t.Fatalf("retained seats = %d", seats)
	}
	res = syncOrg(t, s, orgfixture.Manifest(t))
	if _, err := s.SendSeatMessage(ctx, lease(t, s, res.Seats["lead"].SeatID), SeatMessage{OrganizationID: res.OrganizationID,
		SenderID: res.Seats["lead"].SeatID, RecipientID: res.Seats["engineer"].SeatID, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrganization(ctx, res.OrganizationID, true); err != nil {
		t.Fatal(err)
	}
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM seats`).Scan(&seats)
	if seats != 0 {
		t.Fatalf("purge left %d seats", seats)
	}
}

func TestRetiringSeatRefusesMessages(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	org, lead, eng := res.OrganizationID, res.Seats["lead"].SeatID, res.Seats["engineer"].SeatID
	f := lease(t, s, lead)
	sp := orgfixture.Spec()
	delete(sp.Seats, "engineer")
	delete(sp.MessageRoutes, "lead_engineer")
	delete(sp.MessageRoutes, "engineer_to_reviewer")
	if _, err := s.SyncOrganization(ctx, SyncInput{Namespace: "acme", Key: "acme", Manifest: orgfixture.Compile(t, sp),
		RetirementGrace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SendSeatMessage(ctx, f, SeatMessage{OrganizationID: org, SenderID: lead, RecipientID: eng, Body: "late"})
	if !errors.Is(err, ErrRecipientRetiring) {
		t.Fatalf("send to retiring seat: %v", err)
	}
	if _, err := s.CreateProbe(ctx, org, eng); !errors.Is(err, ErrNotFound) {
		t.Fatalf("probe of retiring seat: %v", err)
	}
	if due, err := s.DueRetirements(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("seat with an unhandled notice is due: %+v %v", due, err)
	}
	rt, err := s.SeatRuntimes(ctx, org, []string{"engineer"})
	if err != nil || rt["engineer"].RetireBy == nil || rt["engineer"].PendingDeliveries != 1 {
		t.Fatalf("runtime = %+v %v", rt, err)
	}
}
