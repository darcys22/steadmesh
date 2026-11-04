//go:build linux

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

type launcherFunc func(context.Context, harnesses.Environment, string, func(Observation)) (Observation, error)

func (f launcherFunc) Run(ctx context.Context, env harnesses.Environment, prompt string, emit func(Observation)) (Observation, error) {
	return f(ctx, env, prompt, emit)
}
func lifecycle(t *testing.T, f launcherFunc) (*RestrictedAdapter, harnesses.Environment) {
	t.Helper()
	a := NewRestricted(ExperimentalLauncher{NodePath: "/opt/node", ACPPath: "/opt/acp", CodexPath: "/opt/codex", NeutralDir: "/opt/neutral", PrivateParent: "/seat/private"})
	a.launcher = f
	env := planEnv()
	env.RunnerDir = t.TempDir()
	return a, env
}
func prepareStart(t *testing.T, a *RestrictedAdapter, env harnesses.Environment, rec harnesses.RecoveryDescriptor) harnesses.ResumeResult {
	t.Helper()
	if err := a.Prepare(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	r, err := a.StartOrResume(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func delivery() harnesses.Delivery {
	return harnesses.Delivery{ExecutionID: "exec", Message: runtimeapi.Envelope{MessageID: "message", Body: "task"}}
}

func TestRestrictedLifecycleFreshDeliveriesAndPortableCheckpoint(t *testing.T) {
	calls := 0
	a, env := lifecycle(t, func(ctx context.Context, e harnesses.Environment, p string, emit func(Observation)) (Observation, error) {
		calls++
		if e.Model.ID != "test-model" || e.TokenFile != "/seat/token" || e.RunnerDir == "" {
			t.Fatal("seat authority changed")
		}
		if !strings.Contains(p, "neutral directory") || !strings.Contains(p, "message_id: message") {
			t.Fatal("missing turn context")
		}
		if e.Bootstrap.Recovery.Handoff.Objective != "recover" || e.Bootstrap.Recovery.UnknownOperations[0].ID != "unknown" {
			t.Fatal("missing explicit recovery descriptor")
		}
		emit(Observation{Kind: "output", Text: "hel"})
		emit(Observation{Kind: "tool_call", ToolCallID: "tool"})
		emit(Observation{Kind: "tool_call_update", ToolCallID: "tool"})
		emit(Observation{Kind: "output", Text: "lo"})
		return Observation{Kind: "stop", StopReason: "end_turn"}, nil
	})
	h := &runtimeapi.Handoff{Objective: "recover"}
	rec := harnesses.RecoveryDescriptor{Session: &runtimeapi.Checkpoint{CheckpointRef: "native-session"}, Handoff: h, UnknownOperations: []runtimeapi.Operation{{ID: "unknown"}}}
	r := prepareStart(t, a, env, rec)
	if calls != 0 || r.Mode != harnesses.RecoveryPortableHandoff || r.SessionID != "" {
		t.Fatal("startup launched or claimed native session")
	}
	env.Model.ID = "mutated"
	h.Objective = "mutated"
	rec.UnknownOperations[0].ID = "mutated"
	for i := 0; i < 2; i++ {
		res, err := a.Deliver(context.Background(), delivery())
		if err != nil || res.Status != harnesses.TurnCompleted || res.Output != "hello" || res.SessionID != "" {
			t.Fatalf("turn: %+v %v", res, err)
		}
	}
	if calls != 2 {
		t.Fatal("must invoke one fresh launcher per delivery")
	}
	cp, err := a.Checkpoint(context.Background())
	if err != nil || cp.CheckpointRef != "none" || cp.FormatVersion != portableFormat {
		t.Fatalf("checkpoint: %+v %v", cp, err)
	}
	raw, err := os.ReadFile(filepath.Join(env.RunnerDir, "acp-portable-checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved runtimeapi.Checkpoint
	if json.Unmarshal(raw, &saved) != nil || saved != cp {
		t.Fatal("descriptor not persisted")
	}
	if a.Events() != a.Events() {
		t.Fatal("unstable event stream")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	completions := 0
	for event := range a.Events() {
		if event.MessageID != "message" || event.ExecutionID != "exec" {
			t.Fatal("uncorrelated event")
		}
		if event.Kind == harnesses.EventCompletion {
			completions++
		}
		if event.Kind == harnesses.EventToolResult {
			t.Fatal("tool update claimed result")
		}
	}
	if completions != 2 {
		t.Fatal("missing completion events")
	}
	if _, err := a.Deliver(context.Background(), delivery()); !errors.Is(err, harnesses.ErrStopped) {
		t.Fatal(err)
	}
}

func TestRestrictedRejectsUnsupportedPrepare(t *testing.T) {
	tests := map[string]func(*harnesses.Environment){
		"static execution": func(e *harnesses.Environment) { e.ExtraEnv = []string{"STEADMESH_EXECUTION=stale"} },
		"execution file":   func(e *harnesses.Environment) { e.ExtraEnv = []string{"STEADMESH_EXECUTION_FILE=/stale"} },
		"config":           func(e *harnesses.Environment) { e.HarnessConfig = map[string]string{"binary": "/evil"} },
		"mcp": func(e *harnesses.Environment) {
			e.MCPServers = []harnesses.MCPServer{{Name: "extra", Command: "/extra"}}
		},
		"skills": func(e *harnesses.Environment) {
			e.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
				return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{}}}, nil
			}
		},
		"skill error": func(e *harnesses.Environment) {
			e.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
				return runtimeapi.SkillsBundleResponse{}, errors.New("private")
			}
		},
		"model": func(e *harnesses.Environment) { e.Model.BaseURL = "https://api.openai.com" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			a, e := lifecycle(t, func(context.Context, harnesses.Environment, string, func(Observation)) (Observation, error) {
				t.Fatal("launched")
				return Observation{}, nil
			})
			change(&e)
			if err := a.Prepare(context.Background(), e); err == nil {
				t.Fatal("accepted")
			}
			a.Stop(context.Background())
		})
	}
}

func TestRestrictedSkillsRefreshedBeforeEveryLaunch(t *testing.T) {
	var revoked atomic.Bool
	calls := 0
	a, e := lifecycle(t, func(context.Context, harnesses.Environment, string, func(Observation)) (Observation, error) {
		calls++
		return Observation{Kind: "stop", StopReason: "end_turn"}, nil
	})
	e.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		if revoked.Load() {
			return runtimeapi.SkillsBundleResponse{}, errors.New("secret")
		}
		return runtimeapi.SkillsBundleResponse{}, nil
	}
	prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
	a.Deliver(context.Background(), delivery())
	revoked.Store(true)
	res, err := a.Deliver(context.Background(), delivery())
	if err != nil || res.Status != harnesses.TurnFailed || calls != 1 || strings.Contains(res.Error, "secret") {
		t.Fatalf("revocation: %+v %v calls=%d", res, err, calls)
	}
	a.Stop(context.Background())
}

func TestRestrictedStopReasons(t *testing.T) {
	for reason, status := range map[string]string{"end_turn": harnesses.TurnCompleted, "cancelled": harnesses.TurnInterrupted, "refusal": harnesses.TurnFailed, "max_tokens": harnesses.TurnFailed, "max_turn_requests": harnesses.TurnFailed, "unknown": harnesses.TurnFailed} {
		t.Run(reason, func(t *testing.T) {
			a, e := lifecycle(t, func(context.Context, harnesses.Environment, string, func(Observation)) (Observation, error) {
				return Observation{Kind: "stop", StopReason: reason}, nil
			})
			prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
			res, _ := a.Deliver(context.Background(), delivery())
			if res.Status != status {
				t.Fatalf("%+v", res)
			}
			a.Stop(context.Background())
		})
	}
}

func TestRestrictedConcurrentQuiesceAndStop(t *testing.T) {
	for _, mode := range []string{"quiesce", "stop", "deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			exited := make(chan harnesses.TurnResult, 1)
			a, e := lifecycle(t, func(ctx context.Context, _ harnesses.Environment, _ string, emit func(Observation)) (Observation, error) {
				close(entered)
				<-ctx.Done()
				emit(Observation{Kind: "output", Text: "last"})
				return Observation{Kind: "stop", StopReason: "end_turn"}, nil
			})
			prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			go func() { res, _ := a.Deliver(ctx, delivery()); exited <- res }()
			<-entered
			if _, err := a.Deliver(context.Background(), delivery()); !errors.Is(err, harnesses.ErrBusy) {
				t.Fatal(err)
			}
			switch mode {
			case "quiesce":
				qctx, qcancel := context.WithCancel(context.Background())
				qcancel()
				r, _ := a.Quiesce(qctx)
				if !r.Interrupted {
					t.Fatal("not interrupted")
				}
				if _, err := a.Deliver(context.Background(), delivery()); !errors.Is(err, harnesses.ErrQuiescing) {
					t.Fatal(err)
				}
			case "stop":
				if err := a.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			select {
			case res := <-exited:
				want := harnesses.TurnInterrupted
				if mode == "deadline" {
					want = harnesses.TurnTimedOut
				}
				if res.Status != want {
					t.Fatalf("cancellation lost to terminal result: %+v", res)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("turn stuck")
			}
			a.Stop(context.Background())
			a.Stop(context.Background())
			last := ""
			for event := range a.Events() {
				last = event.Kind
			}
			if last != harnesses.EventCompletion {
				t.Fatal("closed before completion")
			}
		})
	}
}

func TestRestrictedCapabilitiesAndState(t *testing.T) {
	a, e := lifecycle(t, nil)
	c := a.DescribeCapabilities()
	if c.Adapter == "acp-experimental" || c.GracefulSuspension || c.SessionFormat != "" || strings.Contains(strings.Join(c.RecoveryModes, ","), "native") {
		t.Fatalf("unsupported capability: %+v", c)
	}
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err == nil {
		t.Fatal("start before prepare")
	}
	if _, err := a.Deliver(context.Background(), delivery()); !errors.Is(err, harnesses.ErrNotStarted) {
		t.Fatal(err)
	}
	if _, err := a.Checkpoint(context.Background()); !errors.Is(err, harnesses.ErrNotStarted) {
		t.Fatal(err)
	}
	r := prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
	if r.Mode != harnesses.RecoveryFresh {
		t.Fatal(r)
	}
	if err := a.Prepare(context.Background(), e); err == nil {
		t.Fatal("reprepare")
	}
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err == nil {
		t.Fatal("restart")
	}
	a.Stop(context.Background())
	if err := a.Prepare(context.Background(), e); !errors.Is(err, harnesses.ErrStopped) {
		t.Fatal(err)
	}
}

func TestRestrictedStopUnblocksUndrainedEvents(t *testing.T) {
	entered := make(chan struct{})
	a, e := lifecycle(t, func(ctx context.Context, _ harnesses.Environment, _ string, emit func(Observation)) (Observation, error) {
		close(entered)
		for i := 0; i < 300; i++ {
			emit(Observation{Kind: "output", Text: "x"})
		}
		return Observation{}, ctx.Err()
	})
	prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
	done := make(chan struct{})
	go func() { a.Deliver(context.Background(), delivery()); close(done) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("backpressure kept turn alive")
	}
	for range a.Events() {
	}
}

func TestRestrictedStopDeadlineDoesNotCloseBeforeCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a, e := lifecycle(t, func(ctx context.Context, _ harnesses.Environment, _ string, _ func(Observation)) (Observation, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return Observation{}, ctx.Err()
	})
	prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
	done := make(chan struct{})
	go func() { a.Deliver(context.Background(), delivery()); close(done) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(a.Stop(ctx), context.Canceled) {
		t.Fatal("deadline was not reported")
	}
	// Drain the existing start event; the channel must still be open.
	<-a.Events()
	select {
	case _, ok := <-a.Events():
		if !ok {
			t.Fatal("closed during process cleanup")
		}
	default:
	}
	close(release)
	<-done
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := ""
	for event := range a.Events() {
		last = event.Kind
	}
	if last != harnesses.EventCompletion {
		t.Fatal("lost terminal event")
	}
}

func TestRestrictedPerTurnContextAndStructuredValidation(t *testing.T) {
	calls := 0
	a, e := lifecycle(t, func(_ context.Context, _ harnesses.Environment, prompt string, emit func(Observation)) (Observation, error) {
		calls++
		if !strings.Contains(prompt, "output_schema_json") {
			t.Fatal("schema not rendered")
		}
		if calls == 1 {
			if !strings.Contains(prompt, "first-context") {
				t.Fatal("missing context")
			}
			emit(Observation{Kind: "output", Text: `{"ok":true}`})
		} else {
			if strings.Contains(prompt, "first-context") || !strings.Contains(prompt, "second-context") {
				t.Fatal("context not refreshed")
			}
			emit(Observation{Kind: "output", Text: `{"ok":"wrong type"}`})
		}
		return Observation{Kind: "stop", StopReason: "end_turn"}, nil
	})
	prepareStart(t, a, e, harnesses.RecoveryDescriptor{})
	defer a.Stop(context.Background())
	d := delivery()
	d.OutputSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	d.Context = &runtimeapi.ParticipantContext{Revision: 1, Summary: "first-context"}
	res, err := harnesses.DeliverValidated(context.Background(), a, d)
	if err != nil || res.Status != harnesses.TurnCompleted || string(res.StructuredOutput) != `{"ok":true}` || calls != 1 {
		t.Fatalf("valid output: %+v %v calls=%d", res, err, calls)
	}
	d.Context = &runtimeapi.ParticipantContext{Revision: 2, Summary: "second-context"}
	res, err = harnesses.DeliverValidated(context.Background(), a, d)
	if !errors.Is(err, harnesses.ErrStructuredOutput) || res.Status != harnesses.TurnFailed || calls != 2 {
		t.Fatalf("invalid output/replay: %+v %v calls=%d", res, err, calls)
	}
}
