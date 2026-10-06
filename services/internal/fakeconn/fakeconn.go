// Package fakeconn provides in-process connector adapters for service tests.
package fakeconn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/darcys22/steadmesh/connectors"
)

// Tracker is a scripted work tracker. Script holds the error returned by each
// successive Invoke (nil or past the end means success). When an ambiguous
// error is scripted and Landed is set, the effect is applied anyway, as when
// a response is lost after the request was accepted.
type Tracker struct {
	mu        sync.Mutex
	Script    []error
	Landed    bool
	VerifyErr error
	calls     []string
	applied   map[string]connectors.Result
}

func (t *Tracker) Verify(context.Context) error { return t.VerifyErr }

func (t *Tracker) Invoke(_ context.Context, op string, params json.RawMessage, operationID string) (connectors.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.calls)
	t.calls = append(t.calls, operationID)
	var err error
	if n < len(t.Script) {
		err = t.Script[n]
	}
	res := connectors.Result{Receipt: "TRK-" + operationID[:8], Data: json.RawMessage(`{"op":"` + op + `"}`)}
	if err == nil || (errors.Is(err, connectors.ErrAmbiguous) && t.Landed) {
		if t.applied == nil {
			t.applied = map[string]connectors.Result{}
		}
		t.applied[operationID] = res
	}
	if err != nil {
		return connectors.Result{}, err
	}
	return res, nil
}

func (t *Tracker) FindByOperation(_ context.Context, _ string, _ json.RawMessage, operationID string) (*connectors.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.applied[operationID]; ok {
		return &r, nil
	}
	return nil, nil
}

func (t *Tracker) ReadOnly(op string) bool { return strings.HasSuffix(op, ".read") }

// Calls returns the operation ids passed to Invoke.
func (t *Tracker) Calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

// Applied reports how many distinct effects landed.
func (t *Tracker) Applied() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.applied)
}

// Comm is a communication adapter fed through Events.
type Comm struct {
	Key       string
	Events    chan connectors.InboundEvent
	Users     map[string]bool
	VerifyErr error
	// SendErrs holds the error returned by each successive Send.
	SendErrs []error

	mu       sync.Mutex
	healthy  bool
	sent     []connectors.OutboundMessage
	accepted chan error
}

// NewComm returns a Comm whose sink results are reported on Accepted.
func NewComm(users ...string) *Comm {
	c := &Comm{Events: make(chan connectors.InboundEvent), Users: map[string]bool{}, accepted: make(chan error, 64)}
	for _, u := range users {
		c.Users[u] = true
	}
	return c
}

// Factory returns an adapter factory yielding c.
func (c *Comm) Factory() func(connectors.Config) (connectors.Communication, error) {
	return func(cfg connectors.Config) (connectors.Communication, error) {
		c.Key = cfg.Key
		return c, nil
	}
}

func (c *Comm) Verify(context.Context) error { return c.VerifyErr }

func (c *Comm) VerifyUser(_ context.Context, id string) error {
	if !c.Users[id] {
		return errors.New("users.info: user_not_found")
	}
	return nil
}

func (c *Comm) Run(ctx context.Context, sink connectors.IngressSink) error {
	c.setHealthy(true)
	defer c.setHealthy(false)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.Events:
			c.accepted <- sink.Accept(ctx, c.Key, ev)
		}
	}
}

// Accepted returns the sink's result for the next event.
func (c *Comm) Accepted() <-chan error { return c.accepted }

func (c *Comm) setHealthy(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.healthy = v
}

func (c *Comm) Healthy() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.healthy {
		return false, "socket not connected"
	}
	return true, ""
}

func (c *Comm) Send(_ context.Context, msg connectors.OutboundMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.sent)
	c.sent = append(c.sent, msg)
	if n < len(c.SendErrs) && c.SendErrs[n] != nil {
		return "", c.SendErrs[n]
	}
	return "ts-" + msg.IdempotencyKey[:8], nil
}

// Sent returns the messages passed to Send.
func (c *Comm) Sent() []connectors.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connectors.OutboundMessage(nil), c.sent...)
}

// Model echoes proxied requests so tests can inspect what reached the provider.
type Model struct{}

func (Model) Verify(context.Context) error { return nil }

func (Model) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path": r.URL.Path, "authorization": r.Header.Get("Authorization"), "x_api_key": r.Header.Get("X-Api-Key"),
		})
	})
}

// Secrets resolves every reference to a fixed value.
type Secrets struct{ Err error }

func (s Secrets) Resolve(context.Context, string) (map[string]string, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	return map[string]string{"api_key": "test"}, nil
}
