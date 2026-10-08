package terminal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

type recordingSink struct {
	mu     sync.Mutex
	events []connectors.InboundEvent
	fail   bool
}

func (s *recordingSink) Accept(_ context.Context, _ string, ev connectors.InboundEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("store unavailable")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingSink) snapshot() []connectors.InboundEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connectors.InboundEvent{}, s.events...)
}

func start(t *testing.T) (*Adapter, *recordingSink, *httptest.Server) {
	t.Helper()
	a, err := newAdapter(connectors.Config{Key: "terminal", AccountID: "local",
		Secret: map[string]string{"sean": "tok-sean", "ana": "tok-ana"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx, sink); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { ok, _ := a.Healthy(); return ok })
	return a, sink, srv
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

func post(t *testing.T, srv *httptest.Server, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/messages", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// stream opens GET /events and returns a channel of decoded events.
func stream(t *testing.T, srv *httptest.Server, token string) <-chan Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status %d", resp.StatusCode)
	}
	out := make(chan Event, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(data), &ev) == nil {
				out <- ev
			}
		}
	}()
	return out
}

func next(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func TestNewRequiresTokens(t *testing.T) {
	if _, err := New(connectors.Config{Secret: map[string]string{"sean": " "}}); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestVerifyUser(t *testing.T) {
	a, _, _ := start(t)
	if err := a.VerifyUser(context.Background(), "sean"); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyUser(context.Background(), "bob"); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestPostAuthenticatesAndAccepts(t *testing.T) {
	_, sink, srv := start(t)
	if got := post(t, srv, "", `{"id":"1","text":"hi"}`); got != http.StatusUnauthorized {
		t.Fatalf("no token: %d", got)
	}
	if got := post(t, srv, "wrong", `{"id":"1","text":"hi"}`); got != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", got)
	}
	if got := post(t, srv, "tok-sean", `{"id":"1","text":" "}`); got != http.StatusBadRequest {
		t.Fatalf("empty text: %d", got)
	}
	if got := post(t, srv, "tok-ana", `{"id":"1","text":"hello"}`); got != http.StatusAccepted {
		t.Fatalf("post: %d", got)
	}
	evs := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events = %v", evs)
	}
	ev := evs[0]
	if ev.UserID != "ana" || ev.EventID != "terminal:ana:1" || ev.ChannelID != ChannelID || ev.AccountID != "local" || ev.Text != "hello" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestPostNotAcknowledgedWhenSinkFails(t *testing.T) {
	_, sink, srv := start(t)
	sink.mu.Lock()
	sink.fail = true
	sink.mu.Unlock()
	if got := post(t, srv, "tok-sean", `{"id":"1","text":"hi"}`); got != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", got)
	}
}

func TestPostBeforeRun(t *testing.T) {
	a, err := newAdapter(connectors.Config{Secret: map[string]string{"sean": "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if got := post(t, srv, "tok", `{"id":"1","text":"hi"}`); got != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", got)
	}
	if ok, _ := a.Healthy(); ok {
		t.Fatal("healthy before Run")
	}
}

func TestSendStreamsQueuesAndDeduplicates(t *testing.T) {
	a, _, srv := start(t)
	ctx := context.Background()
	send := func(user, key, text string) {
		t.Helper()
		receipt, err := a.Send(ctx, connectors.OutboundMessage{ExternalUserID: user, ChannelID: ChannelID, Text: text, IdempotencyKey: key})
		if err != nil || receipt != key {
			t.Fatalf("send: %q %v", receipt, err)
		}
	}
	// Offline: queued until a terminal connects.
	send("sean", "op-1", "while you were away")
	send("sean", "op-1", "while you were away")
	ch := stream(t, srv, "tok-sean")
	if ev := next(t, ch); ev.ID != "op-1" || ev.Text != "while you were away" {
		t.Fatalf("backlog = %+v", ev)
	}
	// Online: streamed; another user's message does not leak.
	send("ana", "op-2", "for ana")
	send("sean", "op-3", "live")
	if ev := next(t, ch); ev.ID != "op-3" {
		t.Fatalf("live = %+v", ev)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := a.Send(ctx, connectors.OutboundMessage{ExternalUserID: "bob", Text: "x", IdempotencyKey: "op-4"}); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("unknown user: %v", err)
	}
}
