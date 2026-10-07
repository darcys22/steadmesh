package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
)

// keyring is the upstream service's view of which API keys are valid.
type keyring struct {
	mu     sync.Mutex
	valid  map[string]bool
	builds atomic.Int32
}

func newKeyring(keys ...string) *keyring {
	k := &keyring{valid: map[string]bool{}}
	for _, key := range keys {
		k.valid[key] = true
	}
	return k
}

func (k *keyring) set(key string, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.valid[key] = ok
}

func (k *keyring) check(key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.valid[key] {
		return fmt.Errorf("%w: key rejected", connectors.ErrUnauthorized)
	}
	return nil
}

// keyedTracker uses the API key it was built with. Invoke blocks on gate
// when set, to hold a request in flight.
type keyedTracker struct {
	key  string
	ring *keyring
	gate chan struct{}
}

func (t *keyedTracker) Verify(context.Context) error { return t.ring.check(t.key) }
func (t *keyedTracker) Invoke(context.Context, string, json.RawMessage, string) (connectors.Result, error) {
	if t.gate != nil {
		<-t.gate
	}
	if err := t.ring.check(t.key); err != nil {
		return connectors.Result{}, err
	}
	return connectors.Result{Receipt: "used:" + t.key}, nil
}
func (t *keyedTracker) FindByOperation(context.Context, string, json.RawMessage, string) (*connectors.Result, error) {
	return nil, nil
}
func (t *keyedTracker) ReadOnly(string) bool { return false }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const trackerRef = "k8s:linear"

type rig struct {
	t       *testing.T
	ctx     context.Context
	m       *connections.Manager
	secrets *fakeconn.Secrets
	ring    *keyring
	clock   *clock
	gate    chan struct{}
	conns   map[string]spec.Connection
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, ctx: ctx, secrets: &fakeconn.Secrets{}, ring: newKeyring("k1"),
		clock: &clock{t: time.Unix(1_700_000_000, 0)},
		conns: map[string]spec.Connection{"tracker": {Adapter: "linear", SecretRef: trackerRef}}}
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k1"})
	r.m = connections.New(ctx, connections.Options{
		Factories: connections.Factories{Tracker: map[string]func(connectors.Config) (connectors.Tracker, error){
			"linear": func(cfg connectors.Config) (connectors.Tracker, error) {
				r.ring.builds.Add(1)
				if cfg.Secret["api_key"] == "" {
					return nil, fmt.Errorf("%w: api_key missing", connectors.ErrPermanent)
				}
				return &keyedTracker{key: cfg.Secret["api_key"], ring: r.ring, gate: r.gate}, nil
			}}},
		Secrets: r.secrets, Sinks: func(string) connectors.IngressSink { return nopSink{} },
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		RefreshInterval: time.Hour, // tests drive refreshes explicitly
		Grace:           10 * time.Minute, MaxStale: time.Hour, Now: r.clock.now,
	})
	t.Cleanup(func() { cancel(); r.m.Close() })
	r.m.Sync(ctx, "org", r.conns)
	return r
}

// refresh forces a refresh, stepping past the rate limit.
func (r *rig) refresh() bool {
	r.clock.add(connections.ForcedRefreshInterval)
	return r.m.RefreshNow(r.ctx, "org", "tracker")
}

func (r *rig) keyInUse() (string, error) {
	tr, err := r.m.Tracker("org", "tracker")
	if err != nil {
		return "", err
	}
	res, err := tr.Invoke(r.ctx, "task.write", nil, "op")
	return strings.TrimPrefix(res.Receipt, "used:"), err
}

func (r *rig) state() runtimeapi.CredentialStatus { return r.m.Credentials("org")["tracker"] }

func (r *rig) wantKey(want string) {
	r.t.Helper()
	got, err := r.keyInUse()
	if err != nil || got != want {
		r.t.Fatalf("key in use = %q, %v; want %q (state %+v)", got, err, want, r.state())
	}
}

func (r *rig) wantUnavailable(substr string) {
	r.t.Helper()
	_, err := r.m.Tracker("org", "tracker")
	if !errors.Is(err, connections.ErrNotConfigured) || !strings.Contains(err.Error(), substr) {
		r.t.Fatalf("want unavailable mentioning %q, got %v", substr, err)
	}
	if st := r.state(); st.State != runtimeapi.CredentialUnavailable {
		r.t.Fatalf("state = %+v", st)
	}
}

func TestRotationActivatesValidReplacement(t *testing.T) {
	r := newRig(t)
	r.wantKey("k1")
	if st := r.state(); st.State != runtimeapi.CredentialCurrent || st.SecretVersion == "" {
		t.Fatalf("initial state %+v", st)
	}
	r.ring.set("k2", true)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k2"})
	if !r.refresh() {
		t.Fatal("valid replacement not activated")
	}
	r.wantKey("k2")
	// The old key may now be revoked without affecting anything.
	r.ring.set("k1", false)
	r.wantKey("k2")
}

func TestRotationIgnoresMetadataOnlyChanges(t *testing.T) {
	r := newRig(t)
	builds := r.ring.builds.Load()
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k1"}) // new version, same content
	if r.refresh() {
		t.Fatal("unchanged content reported as a new credential")
	}
	if r.ring.builds.Load() != builds {
		t.Fatal("unchanged content rebuilt the adapter")
	}
	r.wantKey("k1")
}

func TestRotationRejectedReplacementKeepsPreviousForGraceThenFails(t *testing.T) {
	r := newRig(t)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "bad"})
	if r.refresh() {
		t.Fatal("invalid replacement activated")
	}
	st := r.state()
	if st.State != runtimeapi.CredentialReplacementRejected || st.PreviousUntil == nil || !strings.Contains(st.Error, "rejected") {
		t.Fatalf("state %+v", st)
	}
	r.wantKey("k1") // previous credential still serves
	// Re-resolving the same rejected content does not retry validation.
	builds := r.ring.builds.Load()
	r.refresh()
	if r.ring.builds.Load() != builds {
		t.Fatal("rejected content validated again")
	}
	// After the grace period the connection is unavailable, never switched
	// to the known-invalid credential.
	r.clock.add(10 * time.Minute)
	r.refresh()
	r.wantUnavailable("rejected")
	// Sync must not resurrect it either.
	r.m.Sync(r.ctx, "org", r.conns)
	r.wantUnavailable("rejected")
	// A valid replacement recovers automatically.
	r.ring.set("k3", true)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k3"})
	if !r.refresh() {
		t.Fatal("valid replacement not activated after unavailability")
	}
	r.wantKey("k3")
	if st := r.state(); st.State != runtimeapi.CredentialCurrent || st.Error != "" || st.PreviousUntil != nil {
		t.Fatalf("state after recovery %+v", st)
	}
}

func TestRotationRevokedKeyThenInvalidReplacementFailsAtOnce(t *testing.T) {
	r := newRig(t)
	r.ring.set("k1", false) // revoked upstream
	r.secrets.Set(trackerRef, map[string]string{"api_key": "bad"})
	r.refresh()
	r.wantUnavailable("previous credential is also rejected")
	r.ring.set("k2", true)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k2"})
	if !r.refresh() {
		t.Fatal("did not recover")
	}
	r.wantKey("k2")
}

func TestRotationDeletedSecret(t *testing.T) {
	r := newRig(t)
	r.secrets.Set(trackerRef, nil)
	r.refresh()
	if st := r.state(); st.State != runtimeapi.CredentialSecretMissing || st.PreviousUntil == nil {
		t.Fatalf("state %+v", st)
	}
	r.wantKey("k1")
	r.clock.add(11 * time.Minute)
	r.refresh()
	r.wantUnavailable("missing")
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k1"})
	if !r.refresh() {
		t.Fatal("recreated secret not picked up")
	}
	r.wantKey("k1")
}

func TestRotationSecretStoreOutage(t *testing.T) {
	r := newRig(t)
	r.secrets.SetErr(fmt.Errorf("%w: vault sealed", connectors.ErrRetryable))
	r.refresh()
	if st := r.state(); st.State != runtimeapi.CredentialRefreshFailing || st.FailingSince == nil {
		t.Fatalf("state %+v", st)
	}
	r.wantKey("k1") // keeps serving
	r.clock.add(time.Hour)
	r.refresh()
	r.wantUnavailable("could not be refreshed")
	r.secrets.SetErr(nil)
	if !r.refresh() {
		t.Fatal("did not recover after the store came back")
	}
	r.wantKey("k1")
}

func TestRotationInFlightRequestFinishesOnPreviousCredential(t *testing.T) {
	r := newRig(t)
	r.gate = make(chan struct{})
	// Rebuild so the adapter in use blocks in Invoke.
	r.ring.set("k1b", true)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k1b"})
	r.refresh()
	old, err := r.m.Tracker("org", "tracker")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		res, err := old.Invoke(r.ctx, "task.write", nil, "inflight")
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- res.Receipt
	}()
	r.ring.set("k2", true)
	r.secrets.Set(trackerRef, map[string]string{"api_key": "k2"})
	r.gate = nil
	if !r.refresh() {
		t.Fatal("rotation blocked by an in-flight request")
	}
	if tr, _ := r.m.Tracker("org", "tracker"); tr == old {
		t.Fatal("new requests still get the previous adapter")
	}
	close(old.(*keyedTracker).gate)
	if got := <-done; got != "used:k1b" {
		t.Fatalf("in-flight request: %s", got)
	}
}

// ---- communication: make-before-break ingress swap ----

// socketHub stands in for Slack Socket Mode: events go to any connected
// socket, and an event not acknowledged before its socket closes is
// delivered again.
type socketHub struct {
	events  chan string
	ring    *keyring
	live    atomic.Int32
	maxLive atomic.Int32
}

type socket struct {
	hub     *socketHub
	key     string
	healthy atomic.Bool
}

func (s *socket) Verify(context.Context) error             { return s.hub.ring.check(s.key) }
func (s *socket) VerifyUser(context.Context, string) error { return nil }
func (s *socket) Healthy() (bool, string)                  { return s.healthy.Load(), "" }
func (s *socket) Send(context.Context, connectors.OutboundMessage) (string, error) {
	return "", nil
}

func (s *socket) Run(ctx context.Context, sink connectors.IngressSink) error {
	if err := s.hub.ring.check(s.key); err != nil {
		return err
	}
	n := s.hub.live.Add(1)
	for m := s.hub.maxLive.Load(); n > m && !s.hub.maxLive.CompareAndSwap(m, n); m = s.hub.maxLive.Load() {
	}
	s.healthy.Store(true)
	defer func() { s.healthy.Store(false); s.hub.live.Add(-1) }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-s.hub.events:
			if err := sink.Accept(ctx, "slack", connectors.InboundEvent{EventID: id}); err != nil || ctx.Err() != nil {
				s.hub.events <- id // not acknowledged: redelivered
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
	}
}

// dedupSink accepts each event ID once, like messages_external_event.
type dedupSink struct {
	mu   sync.Mutex
	seen map[string]int
}

func (d *dedupSink) Accept(_ context.Context, _ string, ev connectors.InboundEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[ev.EventID]++
	return nil
}

func TestRotationSwapsIngressWithoutLosingEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hub := &socketHub{events: make(chan string, 1024), ring: newKeyring("x1")}
	secrets := &fakeconn.Secrets{}
	secrets.Set("k8s:slack", map[string]string{"bot_token": "x1"})
	sink := &dedupSink{seen: map[string]int{}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	m := connections.New(ctx, connections.Options{
		Factories: connections.Factories{Communication: map[string]func(connectors.Config) (connectors.Communication, error){
			"slack": func(cfg connectors.Config) (connectors.Communication, error) {
				return &socket{hub: hub, key: cfg.Secret["bot_token"]}, nil
			}}},
		Secrets: secrets, Sinks: func(string) connectors.IngressSink { return sink },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), RefreshInterval: time.Hour, Now: clk.now,
	})
	defer func() { cancel(); m.Close() }()
	m.Sync(ctx, "org", map[string]spec.Connection{"slack": {Adapter: "slack", SecretRef: "k8s:slack"}})

	const total = 400
	go func() {
		for i := range total {
			hub.events <- fmt.Sprintf("Ev%d", i)
			time.Sleep(200 * time.Microsecond)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	hub.ring.set("x2", true)
	secrets.Set("k8s:slack", map[string]string{"bot_token": "x2"})
	clk.t = clk.t.Add(time.Minute)
	if !m.RefreshNow(ctx, "org", "slack") {
		t.Fatal("rotation not activated")
	}
	if hub.maxLive.Load() < 2 {
		t.Fatal("the replacement socket was not connected before the old one stopped")
	}
	hub.ring.set("x1", false)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.seen)
		sink.mu.Unlock()
		if n == total || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.seen) != total {
		t.Fatalf("accepted %d distinct events, want %d", len(sink.seen), total)
	}
	if live := hub.live.Load(); live != 1 {
		t.Fatalf("%d sockets still connected after the swap, want 1", live)
	}
}
