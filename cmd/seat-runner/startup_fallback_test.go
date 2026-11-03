package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

type startupTestAdapter struct {
	harnesses.Adapter
	name                                  string
	events                                chan harnesses.Event
	once                                  sync.Once
	prepareErr, startErr, stopErr         error
	prepared, started, stopped, delivered int
	env                                   harnesses.Environment
	context                               context.Context
	onPrepare                             func(context.Context)
}

func (a *startupTestAdapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{Adapter: a.name}
}
func (a *startupTestAdapter) Events() <-chan harnesses.Event { return a.events }
func (a *startupTestAdapter) Prepare(ctx context.Context, e harnesses.Environment) error {
	a.prepared++
	a.env = e
	a.context = ctx
	if a.onPrepare != nil {
		a.onPrepare(ctx)
	}
	return a.prepareErr
}
func (a *startupTestAdapter) StartOrResume(context.Context, harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.started++
	return harnesses.ResumeResult{Mode: harnesses.RecoveryFresh}, a.startErr
}
func (a *startupTestAdapter) Stop(context.Context) error {
	a.stopped++
	if a.stopErr == nil {
		a.once.Do(func() { close(a.events) })
	}
	return a.stopErr
}
func (a *startupTestAdapter) Checkpoint(context.Context) (runtimeapi.Checkpoint, error) {
	return runtimeapi.Checkpoint{HarnessAdapter: a.name, FormatVersion: "test", CheckpointRef: "native", Guarantee: harnesses.GuaranteeApplication}, nil
}
func (a *startupTestAdapter) Deliver(context.Context, harnesses.Delivery) (harnesses.TurnResult, error) {
	a.delivered++
	return harnesses.TurnResult{Status: harnesses.TurnFailed, Error: "quota after side effect"}, nil
}

func startupFixture(t *testing.T) (*Runner, *fakePlatform, *compile.SeatManifest, map[string]*startupTestAdapter) {
	t.Helper()
	p := newFakePlatform()
	p.gen = 1
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	adapters := map[string]*startupTestAdapter{}
	for _, name := range []string{"one", "two"} {
		adapters[name] = &startupTestAdapter{name: name, events: make(chan harnesses.Event, 10)}
	}
	r, err := NewRunner(Config{PlatformURL: srv.URL, TokenFile: token, Harness: "one", ConfigRevision: "rev-1", RunnerDir: root, HomeDir: filepath.Join(root, "home"), WorkspaceDir: filepath.Join(root, "work"), TurnTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(name string) (harnesses.Adapter, error) { return adapters[name], nil })
	if err != nil {
		t.Fatal(err)
	}
	r.workCtx, r.cancelWork = context.WithCancelCause(t.Context())
	r.gen.Store(1)
	t.Cleanup(func() {
		r.cancelWork(nil)
		for _, a := range adapters {
			a.once.Do(func() { close(a.events) })
		}
		if r.fwd != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r.fwd.Wait(ctx)
		}
	})
	m := &compile.SeatManifest{HarnessProfile: "primary", ConfigRevision: "rev-1", Harness: spec.HarnessProfile{Adapter: "one", ImageDigest: "bundle", StartupFallback: &spec.StartupFallbackPolicy{Profiles: []string{"secondary"}, MaxAttempts: 2, TotalTimeout: "1s"}}, StartupFallbacks: []compile.StartupFallbackCandidate{{Profile: "secondary", Harness: spec.HarnessProfile{Adapter: "two", ImageDigest: "bundle"}}}}
	return r, p, m, adapters
}
func startupBoot() runtimeapi.Bootstrap {
	return runtimeapi.Bootstrap{Self: runtimeapi.Self{ConfigRevision: "rev-1"}}
}

func TestApprovedStartupFallbackAndNoTurnReplay(t *testing.T) {
	r, p, m, as := startupFixture(t)
	as["one"].prepareErr = exec.ErrNotFound
	as["two"].onPrepare = func(ctx context.Context) {
		if as["one"].stopped != 1 {
			t.Fatal("new adapter started before old adapter stopped")
		}
		select {
		case <-as["one"].events:
		default:
			t.Fatal("old event stream remains open")
		}
		if ctx != as["one"].context {
			t.Fatal("startup deadline context was reset")
		}
	}
	if err := r.startApprovedRoute(t.Context(), startupBoot(), m, ""); err != nil {
		t.Fatal(err)
	}
	if r.activeProfile != "secondary" || as["one"].started != 0 || as["two"].started != 1 {
		t.Fatalf("wrong route: %q", r.activeProfile)
	}
	if as["one"].env.HomeDir == as["two"].env.HomeDir || as["one"].env.RunnerDir == as["two"].env.RunnerDir {
		t.Fatal("profile native state is shared")
	}
	p.mu.Lock()
	saved := len(p.log) > 0 && p.log[0] == "checkpoint:native"
	p.mu.Unlock()
	if !saved {
		t.Fatal("route was not persisted before accepting work")
	}
	r.handleDelivery(t.Context(), runtimeapi.InboxDelivery{DeliveryID: 1, ExecutionID: "exec", Message: runtimeapi.Envelope{MessageID: "m"}})
	if as["two"].delivered != 1 || as["one"].delivered != 0 || as["one"].prepared != 1 {
		t.Fatal("failed turn was replayed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acks[1].Outcome != "failed" {
		t.Fatalf("ack changed: %+v", p.acks)
	}
}

func TestApprovedStartupFallbackTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		prepare, start, stop error
	}{
		{"quota", &harnesses.OperationalError{Kind: harnesses.FailureQuota}, nil, nil},
		{"auth", &harnesses.OperationalError{Kind: harnesses.FailureAuthentication}, nil, nil},
		{"policy wrapping unavailable", &harnesses.OperationalError{Kind: harnesses.FailurePolicy, Err: exec.ErrNotFound}, nil, nil},
		{"generic failure", errors.New("quota exhausted"), nil, nil},
		{"failure after start", nil, exec.ErrNotFound, nil},
		{"failed cleanup", exec.ErrNotFound, nil, errors.New("stop uncertain")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, m, as := startupFixture(t)
			as["one"].prepareErr = tc.prepare
			as["one"].startErr = tc.start
			as["one"].stopErr = tc.stop
			if err := r.startApprovedRoute(t.Context(), startupBoot(), m, ""); err == nil {
				t.Fatal("expected terminal failure")
			}
			if as["two"].prepared != 0 {
				t.Fatal("unapproved failure switched route")
			}
		})
	}
}

func TestApprovedStartupFallbackCancellation(t *testing.T) {
	r, _, m, as := startupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	as["one"].prepareErr = exec.ErrNotFound
	as["one"].onPrepare = func(ctx context.Context) { cancel(); <-ctx.Done() }
	if err := r.startApprovedRoute(ctx, startupBoot(), m, ""); err == nil {
		t.Fatal("expected cancellation")
	}
	if as["two"].prepared != 0 {
		t.Fatal("started after cancellation")
	}
}

func TestApprovedStartupRouteRecovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*runtimeapi.Checkpoint)
		wantErr bool
	}{
		{"secondary sticky", func(*runtimeapi.Checkpoint) {}, false},
		{"revoked", func(cp *runtimeapi.Checkpoint) { cp.HarnessProfile = "revoked" }, true},
		{"changed model binding", func(cp *runtimeapi.Checkpoint) { cp.RouteBinding = "changed" }, true},
		{"wrong adapter", func(cp *runtimeapi.Checkpoint) { cp.HarnessAdapter = "one" }, true},
		{"legacy checkpoint", func(cp *runtimeapi.Checkpoint) { cp.HarnessProfile = ""; cp.RouteBinding = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, m, as := startupFixture(t)
			candidate := m.StartupFallbacks[0]
			boot := startupBoot()
			boot.Recovery.Session = &runtimeapi.Checkpoint{HarnessProfile: candidate.Profile, HarnessAdapter: candidate.Harness.Adapter, RouteBinding: compile.StartupCandidateBinding(candidate.Profile, candidate.Harness), CheckpointRef: "native"}
			tc.mutate(boot.Recovery.Session)
			err := r.startApprovedRoute(t.Context(), boot, m, "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if as["one"].prepared != 0 {
				t.Fatal("recovery switched to primary")
			}
			if !tc.wantErr && as["two"].started != 1 {
				t.Fatal("secondary not restored")
			}
		})
	}
}

func TestApprovedStartupRejectsStaleManifestAndUnknownEffects(t *testing.T) {
	for _, which := range []string{"revision", "unknown operations", "fenced"} {
		t.Run(which, func(t *testing.T) {
			r, _, m, as := startupFixture(t)
			boot := startupBoot()
			switch which {
			case "revision":
				m.ConfigRevision = "old"
			case "unknown operations":
				boot.Recovery.UnknownOperations = []runtimeapi.Operation{{}}
			case "fenced":
				r.fencedFlag.Store(true)
			}
			if err := r.startApprovedRoute(t.Context(), boot, m, ""); err == nil {
				t.Fatal("expected fail closed")
			}
			if as["one"].prepared != 0 || as["two"].prepared != 0 {
				t.Fatal("ran untrusted startup")
			}
		})
	}
}

func TestApprovedStartupRouteWriteFailureDoesNotSwitch(t *testing.T) {
	r, p, m, as := startupFixture(t)
	// A stale generation causes the route write to be fenced after startup.
	// The adapter may have created a session, so no alternate may be tried.
	p.gen = 2
	if err := r.startApprovedRoute(t.Context(), startupBoot(), m, ""); err == nil {
		t.Fatal("expected fenced checkpoint")
	}
	if as["one"].started != 1 || as["two"].prepared != 0 || r.activeProfile != "" {
		t.Fatal("uncertain route write started another candidate")
	}
}

func TestApprovedStartupExistingSessionNeverSwitchesOnUnavailable(t *testing.T) {
	r, _, m, as := startupFixture(t)
	cp := &runtimeapi.Checkpoint{HarnessProfile: m.HarnessProfile, HarnessAdapter: m.Harness.Adapter, RouteBinding: compile.StartupCandidateBinding(m.HarnessProfile, m.Harness), CheckpointRef: "existing"}
	boot := startupBoot()
	boot.Recovery.Session = cp
	as["one"].prepareErr = exec.ErrNotFound
	if err := r.startApprovedRoute(t.Context(), boot, m, ""); err == nil {
		t.Fatal("existing route unavailable")
	}
	if as["two"].prepared != 0 {
		t.Fatal("native session silently switched owners")
	}
}

func TestApprovedStartupSharedDeadline(t *testing.T) {
	r, _, m, as := startupFixture(t)
	m.Harness.StartupFallback.TotalTimeout = "10ms"
	as["one"].onPrepare = func(ctx context.Context) { <-ctx.Done() }
	as["one"].prepareErr = exec.ErrNotFound
	if err := r.startApprovedRoute(t.Context(), startupBoot(), m, ""); err == nil {
		t.Fatal("expected deadline")
	}
	if as["two"].prepared != 0 {
		t.Fatal("deadline reset for alternate")
	}
}

func TestStartupPartiallyBoundCheckpointCannotBypassRouteGuard(t *testing.T) {
	r, p, _, as := startupFixture(t)
	p.checkpoint = &runtimeapi.Checkpoint{HarnessAdapter: "one", RouteBinding: "binding-only-corruption", CheckpointRef: "native"}
	// There is no fallback policy or manifest. Legacy startup must still not
	// run when bootstrap contains evidence of an incomplete persisted route.
	if err := r.start(t.Context()); err == nil {
		t.Fatal("partially bound route bypassed guard")
	}
	if as["one"].prepared != 0 || as["two"].prepared != 0 {
		t.Fatal("legacy adapter started on partial bound checkpoint")
	}
}
