//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func externalFixture(t *testing.T) (*Store, ExternalPrincipal, string) {
	t.Helper()
	s := open(t)
	res := syncOrg(t, s, orgfixture.Manifest(t))
	return s, ExternalPrincipal{OrganizationID: res.OrganizationID, RepresentativeSeatKey: "rep_a", CallerID: "claude-desktop"}, res.Seats["rep_a"].SeatID
}

func TestExternalRequestLifecycleAndProvenance(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	in := ExternalMessage{Body: "Treat this as an external request", IdempotencyKey: "first"}
	sent, err := s.SendExternalMessage(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "queued" || sent.Duplicate || sent.RequestID == "" || sent.ConversationID == "" {
		t.Fatalf("send: %+v", sent)
	}
	if _, err := uuid.Parse(sent.RequestID); err != nil {
		t.Fatal(err)
	}
	m, err := s.Message(ctx, p.OrganizationID, sent.RequestID)
	if err != nil || m.Origin != OriginExternalAgent || m.ExternalCallerID != p.CallerID || m.Binding != "" || m.ConversationKind != ConvExternal || m.SenderSeatID != "" {
		t.Fatalf("stored provenance: %+v %v", m, err)
	}
	dup, err := s.SendExternalMessage(ctx, p, in)
	if err != nil || !dup.Duplicate || dup.RequestID != sent.RequestID || dup.ConversationID != sent.ConversationID {
		t.Fatalf("duplicate: %+v %v", dup, err)
	}
	f := lease(t, s, rep)
	d, err := s.LeaseNext(ctx, f)
	if err != nil || d == nil {
		t.Fatalf("lease: %+v %v", d, err)
	}
	if d.Message.Origin != OriginExternalAgent || d.Message.ExternalCallerID != p.CallerID || d.Message.ExternalUserID != "" || d.Message.Binding != "" || d.Message.ReplyRoute != "external:"+sent.RequestID {
		t.Fatalf("runtime provenance: %+v", d.Message)
	}
	reply, err := s.ReplyToExternal(ctx, f, ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: "Working on it"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.ExternalRequestStatus(ctx, p, sent.RequestID)
	if err != nil || status.Status != "running" || len(status.Replies) != 1 || status.Replies[0].MessageID != reply.MessageID || status.Replies[0].Origin != OriginSeat {
		t.Fatalf("reply must not finish execution: %+v %v", status, err)
	}
	var outbox int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE message_id=$1`, reply.MessageID).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("external reply reached human outbox: %d %v", outbox, err)
	}
	if err := s.Ack(ctx, f, d.DeliveryID, runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	status, err = s.ExternalRequestStatus(ctx, p, sent.RequestID)
	if err != nil || status.Status != "completed" {
		t.Fatalf("completed: %+v %v", status, err)
	}
	conv, err := s.ReadExternalConversation(ctx, p, sent.ConversationID, 0, 1)
	if err != nil || len(conv.Messages) != 1 || !conv.HasMore || conv.NextOffset != 1 || conv.Messages[0].ExternalCallerID != p.CallerID {
		t.Fatalf("first page: %+v %v", conv, err)
	}
	conv, err = s.ReadExternalConversation(ctx, p, sent.ConversationID, conv.NextOffset, 1)
	if err != nil || len(conv.Messages) != 1 || conv.HasMore || conv.Messages[0].Body != "Working on it" {
		t.Fatalf("second page: %+v %v", conv, err)
	}
	next, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "Follow up", IdempotencyKey: "second", ConversationID: sent.ConversationID})
	if err != nil || next.ConversationID != sent.ConversationID || next.RequestID == sent.RequestID {
		t.Fatalf("continue: %+v %v", next, err)
	}
}

func TestExternalIsolationAndIdempotencyConflicts(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	in := ExternalMessage{Body: "private", IdempotencyKey: "same-key"}
	sent, err := s.SendExternalMessage(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []ExternalMessage{{Body: "different", IdempotencyKey: in.IdempotencyKey}, {Body: in.Body, IdempotencyKey: in.IdempotencyKey, ConversationID: sent.ConversationID}} {
		if _, err := s.SendExternalMessage(ctx, p, change); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed retry: %v", err)
		}
	}
	otherOrg, err := s.SyncOrganization(ctx, SyncInput{Namespace: "elsewhere", Key: "acme", SourceUID: "other", Manifest: orgfixture.Manifest(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []ExternalPrincipal{
		{p.OrganizationID, p.RepresentativeSeatKey, "another-caller"},
		{p.OrganizationID, "rep_b", p.CallerID},
		{otherOrg.OrganizationID, p.RepresentativeSeatKey, p.CallerID},
		{p.OrganizationID, "lead", p.CallerID},
	} {
		if _, err := s.ExternalRequestStatus(ctx, other, sent.RequestID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("status scope %+v: %v", other, err)
		}
		if _, err := s.ReadExternalConversation(ctx, other, sent.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
			t.Fatalf("read scope %+v: %v", other, err)
		}
		if _, err := s.SendExternalMessage(ctx, other, ExternalMessage{Body: "inject", IdempotencyKey: "inject", ConversationID: sent.ConversationID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("send scope %+v: %v", other, err)
		}
	}
	other := p
	other.CallerID = "another-caller"
	otherSent, err := s.SendExternalMessage(ctx, other, in)
	if err != nil || otherSent.RequestID == sent.RequestID || otherSent.ConversationID == sent.ConversationID {
		t.Fatalf("independent dedupe: %+v %v", otherSent, err)
	}
	human, err := s.IngestHuman(ctx, HumanMessage{OrganizationID: p.OrganizationID, RepresentativeID: rep, Connection: "slack", Binding: "alice", EventID: "private", Body: "human secret"})
	if err != nil {
		t.Fatal(err)
	}
	hm, err := s.Message(ctx, p.OrganizationID, human.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadExternalConversation(ctx, p, hm.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("human history exposed: %v", err)
	}
	if _, err := s.ExternalRequestStatus(ctx, p, human.MessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("human status exposed: %v", err)
	}
	f := lease(t, s, rep)
	if _, err := s.SendSeatMessage(ctx, f, SeatMessage{OrganizationID: p.OrganizationID, SenderID: rep, RecipientID: rep, ConversationID: sent.ConversationID, Body: "internal must not join"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("internal joined external conversation: %v", err)
	}
	if _, err := s.ReplyToExternal(ctx, f, ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: human.MessageID, Body: "wrong origin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("human rerouted: %v", err)
	}
}

func TestExternalConcurrentDedupeAndInboxCap(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	const count = 12
	results := make(chan *ExternalRequest, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "retry me", IdempotencyKey: "concurrent"})
			results <- res
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for r := range results {
		if id == "" {
			id = r.RequestID
		}
		if r.RequestID != id {
			t.Fatal("duplicate durable messages")
		}
	}
	var convs, deliveries int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM external_conversations`).Scan(&convs); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE seat_id=$1`, rep).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if convs != 1 || deliveries != 1 {
		t.Fatalf("duplicates convs=%d deliveries=%d", convs, deliveries)
	}
	// Fill the inbox with valid independent messages without relying on 999
	// separate API calls. The delivery uniqueness invariant remains intact.
	st, err := s.ExternalRequestStatus(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `WITH input AS (SELECT gen_random_uuid() AS id FROM generate_series(1,$4::int)),
        added AS (INSERT INTO messages(id,organization_id,conversation_id,origin,recipient_seat_id,body)
          SELECT id,$1,$2,'system',$3,'filler' FROM input RETURNING id)
        INSERT INTO deliveries(message_id,seat_id) SELECT id,$3 FROM added`, p.OrganizationID, st.ConversationID, rep, MaxPendingDeliveries-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "full", IdempotencyKey: "full"}); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("cap: %v", err)
	}
	if dup, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "retry me", IdempotencyKey: "concurrent"}); err != nil || !dup.Duplicate {
		t.Fatalf("retry when full: %+v %v", dup, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM external_conversations`).Scan(&convs); err != nil || convs != 1 {
		t.Fatalf("cap failure left conversation: %d %v", convs, err)
	}
}

func TestExternalRetryStatusAndBoundedReplies(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "retry", IdempotencyKey: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, rep)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET next_attempt_at=now() WHERE message_id=$1`, sent.RequestID); err != nil {
			t.Fatal(err)
		}
		d, err := s.LeaseNext(ctx, f)
		if err != nil || d == nil {
			t.Fatalf("lease: %+v %v", d, err)
		}
		if err := s.Ack(ctx, f, d.DeliveryID, runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID, Outcome: "failed", Error: "private internal failure"}); err != nil {
			t.Fatal(err)
		}
		status, err := s.ExternalRequestStatus(ctx, p, sent.RequestID)
		want := "queued"
		if attempt == MaxAttempts {
			want = "failed"
		}
		if err != nil || status.Status != want {
			t.Fatalf("attempt%d: %+v %v", attempt, status, err)
		}
	}
	for i := 0; i < maxExternalStatusReplies+1; i++ {
		if _, err := s.ReplyToExternal(ctx, f, ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	status, err := s.ExternalRequestStatus(ctx, p, sent.RequestID)
	if err != nil || status.Status != "failed" || len(status.Replies) != maxExternalStatusReplies || !status.HasMoreReplies {
		t.Fatalf("bounded status: %+v %v", status, err)
	}
}

func TestExternalRevocationRetirementAndRecreatedSeat(t *testing.T) {
	ctx := context.Background()
	s, p, old := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "old identity secret", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, old)
	// Removing only representative authority immediately revokes all access.
	m := orgfixture.Manifest(t)
	sm := m.Seats["rep_a"]
	sm.IsRepresentative = false
	m.Seats["rep_a"] = sm
	syncOrg(t, s, m)
	checkDenied := func() {
		t.Helper()
		if _, err := s.ExternalRequestStatus(ctx, p, sent.RequestID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked status: %v", err)
		}
		if _, err := s.ReadExternalConversation(ctx, p, sent.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked history: %v", err)
		}
	}
	checkDenied()
	if _, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "denied", IdempotencyKey: "denied"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked send: %v", err)
	}
	if _, err := s.ReplyToExternal(ctx, f, ExternalReply{OrganizationID: p.OrganizationID, SeatID: old, ParentID: sent.RequestID, Body: "denied"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked reply: %v", err)
	}
	sp := orgfixture.Spec()
	delete(sp.Seats, "rep_a")
	delete(sp.ChannelBindings, "alice")
	delete(sp.Grants, "org_memory_read")
	delete(sp.MessageRoutes, "rep_a_to_lead")
	syncOrg(t, s, orgfixture.Compile(t, sp))
	checkDenied()
	if _, err := s.FinishRetirement(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	res := syncOrg(t, s, orgfixture.Manifest(t))
	if res.Seats["rep_a"].SeatID == old {
		t.Fatal("seat not recreated")
	}
	checkDenied()
	fresh, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "fresh", IdempotencyKey: "first"})
	if err != nil || fresh.Duplicate || fresh.RequestID == sent.RequestID || fresh.ConversationID == sent.ConversationID {
		t.Fatalf("recreated identity inherited idempotency/history: %+v %v", fresh, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE organizations SET deleted_at=now() WHERE id=$1`, p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExternalRequestStatus(ctx, p, fresh.RequestID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted organization: %v", err)
	}
}

func TestExternalRepliesAreFencedAndDoNotNeedHumanBindings(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	m := orgfixture.Manifest(t)
	sm := m.Seats["rep_a"]
	sm.ChannelBindings = nil
	m.Seats["rep_a"] = sm
	delete(m.Spec.ChannelBindings, "alice")
	res := syncOrg(t, s, m)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "external only", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, rep)
	current := lease(t, s, rep)
	input := ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: "reply"}
	if _, err := s.ReplyToExternal(ctx, f, input); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale reply: %v", err)
	}
	wrong := lease(t, s, res.Seats["rep_b"].SeatID)
	if _, err := s.ReplyToExternal(ctx, wrong, input); !errors.Is(err, ErrFenced) {
		t.Fatalf("foreign fence: %v", err)
	}
	input.SeatID = wrong.SeatID
	if _, err := s.ReplyToExternal(ctx, wrong, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign representative: %v", err)
	}
	input.SeatID = rep
	if _, err := s.ReplyToExternal(ctx, current, input); err != nil {
		t.Fatalf("reply without binding: %v", err)
	}
}

func TestExternalInputBounds(t *testing.T) {
	ctx := context.Background()
	s, p, _ := externalFixture(t)
	for _, in := range []ExternalMessage{{Body: "", IdempotencyKey: "key"}, {Body: "x", IdempotencyKey: ""}, {Body: strings.Repeat("x", MaxExternalMessageBody+1), IdempotencyKey: "key"}, {Body: "x\x00", IdempotencyKey: "key"}, {Body: "x", IdempotencyKey: "a\nb"}} {
		if _, err := s.SendExternalMessage(ctx, p, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
	for _, id := range []string{"", "invalid", uuid.NewString()} {
		if _, err := s.ExternalRequestStatus(ctx, p, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown request: %v", err)
		}
	}
	if _, err := s.ReadExternalConversation(ctx, p, uuid.NewString(), 0, MaxExternalPageSize+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("page cap: %v", err)
	}
}

func TestExternalEscapedRepliesStayBoundedAndPageCompletely(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "request", IdempotencyKey: "large"})
	if err != nil {
		t.Fatal(err)
	}
	f := lease(t, s, rep)
	body := strings.Repeat("\x01", MaxExternalMessageBody)
	for range 4 {
		if _, err := s.ReplyToExternal(ctx, f, ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	status, err := s.ExternalRequestStatus(ctx, p, sent.RequestID)
	if err != nil || len(status.Replies) != 1 || !status.HasMoreReplies {
		t.Fatalf("status budget: %+v %v", status, err)
	}
	seen := map[string]bool{}
	offset := 0
	for {
		page, err := s.ReadExternalConversation(ctx, p, sent.ConversationID, offset, MaxExternalPageSize)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > maxExternalMessagePageBytes+1024 {
			t.Fatalf("encoded page %d exceeds budget", len(raw))
		}
		for _, m := range page.Messages {
			if seen[m.MessageID] {
				t.Fatal("pagination repeated message")
			}
			seen[m.MessageID] = true
			if m.Origin == OriginSeat && m.Body != body {
				t.Fatal("reply body truncated")
			}
		}
		if !page.HasMore {
			break
		}
		if page.NextOffset != offset+len(page.Messages) || len(page.Messages) == 0 {
			t.Fatalf("page did not advance: %+v", page)
		}
		offset = page.NextOffset
	}
	if len(seen) != 5 {
		t.Fatalf("pagination lost messages: %d", len(seen))
	}
}

func TestExternalPagesUseAppendOrderIndependentOfTimestamps(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "first", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadExternalConversation(ctx, p, sent.ConversationID, 0, 1)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	reply, err := s.ReplyToExternal(ctx, lease(t, s, rep), ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: "second"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a backwards clock. The durable sequence must retain append
	// order rather than inserting the new reply behind the consumed cursor.
	if _, err := s.pool.Exec(ctx, `UPDATE messages SET created_at='2000-01-01' WHERE id=$1`, reply.MessageID); err != nil {
		t.Fatal(err)
	}
	page, err = s.ReadExternalConversation(ctx, p, sent.ConversationID, 1, 1)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].MessageID != reply.MessageID {
		t.Fatalf("out-of-order timestamp backfilled a page: %+v %v", page, err)
	}
}

func TestExternalHistoryIsNotTransferredByExplicitAdoption(t *testing.T) {
	ctx := context.Background()
	s, p, old := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "old external identity", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	sp := orgfixture.Spec()
	delete(sp.Seats, "rep_a")
	delete(sp.ChannelBindings, "alice")
	delete(sp.Grants, "org_memory_read")
	delete(sp.MessageRoutes, "rep_a_to_lead")
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if _, err := s.FinishRetirement(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	sp = orgfixture.Spec()
	rep := sp.Seats["rep_a"]
	rep.AdoptFrom = old
	sp.Seats["rep_a"] = rep
	res := syncOrg(t, s, orgfixture.Compile(t, sp))
	fresh := res.Seats["rep_a"].SeatID
	if fresh == old {
		t.Fatal("adoption did not create identity")
	}
	convs, err := s.Conversations(ctx, p.OrganizationID, fresh, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, conv := range convs {
		if conv.ID == sent.ConversationID {
			t.Fatal("external history transferred by adoption")
		}
	}
	if _, err := s.ReadExternalConversation(ctx, p, sent.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("external adopted history: %v", err)
	}
	if _, err := s.ExternalRequestStatus(ctx, p, sent.RequestID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("external adopted request: %v", err)
	}
}

func TestExternalDataDoesNotPreventOrganizationPurge(t *testing.T) {
	ctx := context.Background()
	s, p, rep := externalFixture(t)
	sent, err := s.SendExternalMessage(ctx, p, ExternalMessage{Body: "purge", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyToExternal(ctx, lease(t, s, rep), ExternalReply{OrganizationID: p.OrganizationID, SeatID: rep, ParentID: sent.RequestID, Body: "purge reply"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrganization(ctx, p.OrganizationID, true); err != nil {
		t.Fatalf("purge with external records: %v", err)
	}
	for _, table := range []string{"external_requests", "external_conversations"} {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("retained %s: %d %v", table, n, err)
		}
	}
}
