package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	fakeslack "github.com/darcys22/steadmesh/tests/fakes/slack"
)

type recordingSink struct {
	mu       sync.Mutex
	events   []connectors.InboundEvent
	failNext int
	calls    int
}

func (s *recordingSink) Accept(_ context.Context, conn string, ev connectors.InboundEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failNext > 0 {
		s.failNext--
		return errors.New("store unavailable")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingSink) snapshot() ([]connectors.InboundEvent, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connectors.InboundEvent{}, s.events...), s.calls
}

func setup(t *testing.T, opts fakeslack.Options, account string) (*fakeslack.Server, *Adapter) {
	t.Helper()
	if opts.RedeliveryTimeout == 0 {
		opts.RedeliveryTimeout = 300 * time.Millisecond
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = 100 * time.Millisecond
	}
	fake := fakeslack.New(opts)
	srv := httptest.NewServer(fake)
	t.Cleanup(func() { fake.Close(); srv.Close() })
	a, err := newAdapter(connectors.Config{
		Key:       "slack",
		AccountID: account,
		Endpoint:  srv.URL,
		Secret:    map[string]string{"bot_token": "xoxb-test", "app_token": "xapp-test"},
		Extra:     map[string]string{"ping_interval": "2s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fake, a
}

func startRun(t *testing.T, a *Adapter, sink connectors.IngressSink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx, sink); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "connected", func() bool { ok, _ := a.Healthy(); return ok })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func acksFor(fake *fakeslack.Server, eventID string) []fakeslack.Ack {
	var out []fakeslack.Ack
	for _, a := range fake.Acks() {
		if a.EventID == eventID {
			out = append(out, a)
		}
	}
	return out
}

func TestNewRejectsMissingTokens(t *testing.T) {
	_, err := New(connectors.Config{Secret: map[string]string{"bot_token": "xoxb-1"}})
	if !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAPIURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://f:8090":          "http://f:8090/api/",
		"http://f:8090/":         "http://f:8090/api/",
		"http://f:8090/api":      "http://f:8090/api/",
		"https://slack.com/api/": "https://slack.com/api/",
	} {
		if got := apiURL(in); got != want {
			t.Errorf("apiURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerify(t *testing.T) {
	_, a := setup(t, fakeslack.Options{}, "T0FAKE")
	if err := a.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	_, wrong := setup(t, fakeslack.Options{}, "T0OTHER")
	if err := wrong.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("wrong team: want ErrUnauthorized, got %v", err)
	}

	_, badTok := setup(t, fakeslack.Options{BotToken: "xoxb-other"}, "")
	if err := badTok.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("bad token: want ErrUnauthorized, got %v", err)
	}
}

func TestVerifyUser(t *testing.T) {
	_, a := setup(t, fakeslack.Options{}, "T0FAKE")
	if err := a.VerifyUser(context.Background(), "U0ALICE"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := a.VerifyUser(context.Background(), "U0NOBODY"); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("unknown user: want ErrPermanent, got %v", err)
	}
}

func TestRunDeliversDMAndAcksAfterAccept(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	sink := &recordingSink{}
	startRun(t, a, sink)

	inj, err := fake.InjectDM(fakeslack.DM{User: "U0ALICE", Text: "hello rep"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "ack", func() bool { return len(acksFor(fake, inj.EventID)) == 1 })

	evs, _ := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	want := connectors.InboundEvent{
		EventID: inj.EventID, AccountID: "T0FAKE", UserID: "U0ALICE",
		ChannelID: inj.Channel, ThreadRef: inj.Channel, Text: "hello rep", MessageTS: inj.TS,
	}
	if evs[0] != want {
		t.Fatalf("event mismatch:\n got %+v\nwant %+v", evs[0], want)
	}
}

func TestThreadRefUsesThreadTS(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "")
	sink := &recordingSink{}
	startRun(t, a, sink)
	inj, _ := fake.InjectDM(fakeslack.DM{User: "U0BOB", Text: "in thread", ThreadTS: "1700000000.000001"})
	eventually(t, "ack", func() bool { return len(acksFor(fake, inj.EventID)) == 1 })
	evs, _ := sink.snapshot()
	if evs[0].ThreadRef != "1700000000.000001" {
		t.Fatalf("thread ref = %q", evs[0].ThreadRef)
	}
}

func TestNoAckWhenAcceptFailsThenRedelivered(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	sink := &recordingSink{failNext: 1}
	startRun(t, a, sink)

	inj, _ := fake.InjectDM(fakeslack.DM{User: "U0ALICE", Text: "retry me"})
	eventually(t, "ack after redelivery", func() bool { return len(acksFor(fake, inj.EventID)) == 1 })

	acks := acksFor(fake, inj.EventID)
	if acks[0].RetryAttempt != 1 {
		t.Fatalf("ack should be for the redelivery (retry_attempt 1), got %d", acks[0].RetryAttempt)
	}
	evs, calls := sink.snapshot()
	if calls != 2 || len(evs) != 1 || evs[0].EventID != inj.EventID {
		t.Fatalf("calls=%d events=%+v", calls, evs)
	}
}

func TestReplaySameEventIDIsDeliveredWithSameIdentity(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	sink := &recordingSink{}
	startRun(t, a, sink)

	first, _ := fake.InjectDM(fakeslack.DM{User: "U0ALICE", Text: "once", EventID: "Ev0REPLAY"})
	second, _ := fake.InjectDM(fakeslack.DM{User: "U0ALICE", Text: "once", EventID: "Ev0REPLAY"})
	if first.EnvelopeID == second.EnvelopeID {
		t.Fatal("replay should use a new envelope")
	}
	eventually(t, "both acked", func() bool { return len(acksFor(fake, "Ev0REPLAY")) == 2 })
	evs, _ := sink.snapshot()
	if len(evs) != 2 || evs[0].EventID != "Ev0REPLAY" || evs[1].EventID != "Ev0REPLAY" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestIgnoresBotSelfSubtypeAndForeignMessages(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	sink := &recordingSink{}
	startRun(t, a, sink)

	ignored := []fakeslack.DM{
		{User: "U0ALICE", Text: "from a bot", BotID: "B0OTHER"},
		{User: fake.BotUserID(), Text: "our own message"},
		{User: "U0ALICE", Text: "edited", Subtype: "message_changed"},
		{User: "U0ALICE", Text: "other team", TeamID: "T0OTHER"},
	}
	var ids []string
	for _, dm := range ignored {
		inj, _ := fake.InjectDM(dm)
		ids = append(ids, inj.EventID)
	}
	human, _ := fake.InjectDM(fakeslack.DM{User: "U0BOB", Text: "real"})
	eventually(t, "all acked", func() bool {
		for _, id := range append(ids, human.EventID) {
			if len(acksFor(fake, id)) == 0 {
				return false
			}
		}
		return true
	})
	evs, _ := sink.snapshot()
	if len(evs) != 1 || evs[0].EventID != human.EventID {
		t.Fatalf("only the human DM should be accepted, got %+v", evs)
	}
}

func TestReconnectsAfterDisconnect(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	sink := &recordingSink{}
	startRun(t, a, sink)

	fake.Disconnect()
	eventually(t, "reconnected", func() bool {
		ok, _ := a.Healthy()
		return ok && fake.Connections() == 1
	})
	inj, _ := fake.InjectDM(fakeslack.DM{User: "U0ALICE", Text: "after reconnect"})
	eventually(t, "ack", func() bool { return len(acksFor(fake, inj.EventID)) == 1 })
}

func TestHealthyBeforeRun(t *testing.T) {
	_, a := setup(t, fakeslack.Options{}, "")
	if ok, detail := a.Healthy(); ok || detail == "" {
		t.Fatalf("healthy=%v detail=%q", ok, detail)
	}
}

func TestSend(t *testing.T) {
	fake, a := setup(t, fakeslack.Options{}, "T0FAKE")
	ctx := context.Background()

	ts, err := a.Send(ctx, connectors.OutboundMessage{
		ExternalUserID: "U0ALICE", Text: "hi alice", IdempotencyKey: "op-1",
	})
	if err != nil || ts == "" {
		t.Fatalf("send via user: ts=%q err=%v", ts, err)
	}
	ts2, err := a.Send(ctx, connectors.OutboundMessage{
		ChannelID: "D0BOB", ThreadRef: "1700000000.000042", Text: "threaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(ctx, connectors.OutboundMessage{ChannelID: "D0BOB", ThreadRef: "D0BOB", Text: "top level"}); err != nil {
		t.Fatal(err)
	}

	posted := fake.Posted()
	if len(posted) != 3 {
		t.Fatalf("posted = %+v", posted)
	}
	if posted[0].Channel != "D0ALICE" || posted[0].TS != ts || posted[0].ThreadTS != "" {
		t.Fatalf("first = %+v", posted[0])
	}
	var md struct {
		EventType    string            `json:"event_type"`
		EventPayload map[string]string `json:"event_payload"`
	}
	if err := json.Unmarshal(posted[0].Metadata, &md); err != nil {
		t.Fatalf("metadata %s: %v", posted[0].Metadata, err)
	}
	if md.EventType != MetadataEventType || md.EventPayload["operation_id"] != "op-1" {
		t.Fatalf("metadata = %+v", md)
	}
	if posted[1].ThreadTS != "1700000000.000042" || posted[1].TS != ts2 {
		t.Fatalf("second = %+v", posted[1])
	}
	if posted[2].ThreadTS != "" {
		t.Fatalf("channel thread ref must not become thread_ts: %+v", posted[2])
	}
}

func TestSendErrorMapping(t *testing.T) {
	cases := []struct {
		mode      string
		want      error
		committed bool
	}{
		{"rate_limited", connectors.ErrRetryable, false},
		{"invalid_auth", connectors.ErrUnauthorized, false},
		{"token_revoked", connectors.ErrUnauthorized, false},
		{"not_authed", connectors.ErrUnauthorized, false},
		{"ambiguous", connectors.ErrAmbiguous, true},
		{"server_error", connectors.ErrAmbiguous, false},
		{"channel_not_found", connectors.ErrPermanent, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			fake, a := setup(t, fakeslack.Options{}, "")
			fake.Fail("chat.postMessage", tc.mode, 1)
			_, err := a.Send(context.Background(), connectors.OutboundMessage{ChannelID: "D0ALICE", Text: "x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if got := len(fake.Posted()) == 1; got != tc.committed {
				t.Fatalf("committed = %v, want %v", got, tc.committed)
			}
		})
	}
}

func TestSendInvalidInput(t *testing.T) {
	_, a := setup(t, fakeslack.Options{}, "")
	if _, err := a.Send(context.Background(), connectors.OutboundMessage{Text: "nobody"}); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
	if _, err := a.Send(context.Background(), connectors.OutboundMessage{ChannelID: "D0X", Text: " "}); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
}
