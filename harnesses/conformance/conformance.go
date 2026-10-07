// Package conformance is the §7.1 harness adapter conformance suite. Any
// adapter runs it from a test:
//
//	conformance.Run(t, conformance.Options{New: func() harnesses.Adapter { return fake.New() }, ...})
//
// The suite supplies a stub platform tool API (ToolServer) and a freshly
// built steadmesh-tools binary, so adapters exercise the same tool path they
// use in a seat Pod.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Options configures a conformance run.
type Options struct {
	// New returns a fresh, unprepared adapter.
	New func() harnesses.Adapter
	// QuickBody is a message body the adapter completes promptly.
	QuickBody string
	// SlowBody is a message body that keeps the turn running for at least
	// several seconds, used to test interruption and quiesce.
	SlowBody string
	// Configure may adjust the environment (e.g. model proxy URL).
	Configure func(t *testing.T, env *harnesses.Environment)
	// TurnTimeout bounds a quick turn (default 60s).
	TurnTimeout time.Duration
	// InterruptBound is how long an interrupted turn may take to unwind (default 20s).
	InterruptBound time.Duration
}

// ToolCall is one recorded call to the stub platform.
type ToolCall struct {
	Name       string
	Arguments  json.RawMessage
	Generation string
	Execution  string
}

// ToolServer is a stub of the seat-facing tool API (/v1/tools).
type ToolServer struct {
	*httptest.Server
	Token string
	mu    sync.Mutex
	calls []ToolCall
	// Handler optionally overrides a tool's result.
	Handler func(name string, args json.RawMessage) runtimeapi.ToolCallResult
}

// Tools are the descriptors the stub lists.
var Tools = []runtimeapi.ToolDescriptor{
	{Name: "self", Description: "Return the seat identity.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
	{Name: "memory.write", Description: "Create a memory record.", InputSchema: json.RawMessage(`{"type":"object","properties":{"store":{"type":"string"},"path":{"type":"string"},"text":{"type":"string"}},"required":["store","path","text"]}`)},
	{Name: "messages.reply", Description: "Reply to a message.", InputSchema: json.RawMessage(`{"type":"object","properties":{"message_id":{"type":"string"},"binding":{"type":"string"},"body":{"type":"string"}},"required":["body"]}`)},
	{Name: "messages.send", Description: "Send a message to a seat.", InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"body":{"type":"string"},"correlation_id":{"type":"string"}},"required":["to","body"]}`)},
}

// NewToolServer starts a stub tool API that requires token.
func NewToolServer(t testing.TB, token string) *ToolServer {
	ts := &ToolServer{Token: token}
	ts.Server = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.Close)
	return ts
}

// SetToken changes the accepted seat token, as when it is rotated.
func (ts *ToolServer) SetToken(tok string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.Token = tok
}

func (ts *ToolServer) serve(w http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	tok := ts.Token
	ts.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+tok {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(runtimeapi.Error{Code: "unauthenticated", Message: "bad token"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == runtimeapi.PathTools:
		_ = json.NewEncoder(w).Encode(runtimeapi.ToolList{Tools: Tools})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, runtimeapi.PathToolCall):
		name := strings.TrimPrefix(r.URL.Path, runtimeapi.PathToolCall)
		var req runtimeapi.ToolCallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ts.mu.Lock()
		ts.calls = append(ts.calls, ToolCall{Name: name, Arguments: req.Arguments, Generation: r.Header.Get(runtimeapi.HeaderGeneration), Execution: r.Header.Get(runtimeapi.HeaderExecution)})
		n := len(ts.calls)
		h := ts.Handler
		ts.mu.Unlock()
		var res runtimeapi.ToolCallResult
		if h != nil {
			res = h(name, req.Arguments)
		} else {
			res = runtimeapi.ToolCallResult{Content: json.RawMessage(fmt.Sprintf(`{"ok":true,"message_id":"sent-%d","id":"rec-%d"}`, n, n))}
		}
		_ = json.NewEncoder(w).Encode(res)
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(runtimeapi.Error{Code: "not_found", Message: r.URL.Path})
	}
}

// Calls returns the recorded calls.
func (ts *ToolServer) Calls() []ToolCall {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]ToolCall(nil), ts.calls...)
}

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// BuildTools builds cmd/steadmesh-tools once per test binary and returns its path.
func BuildTools(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("STEADMESH_TOOLS_BIN"); p != "" {
		// Prebuilt binary, e.g. when running inside a seat image without Go.
		return p
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "steadmesh-tools-*")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "steadmesh-tools")
		cmd := exec.Command("go", "build", "-o", buildPath, "github.com/darcys22/steadmesh/cmd/steadmesh-tools")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build steadmesh-tools: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

// Dirs are the seat volume directories for one test seat.
type Dirs struct{ Root, Workspace, Home, Runner, Tmp, TokenFile string }

// NewDirs creates a seat-like layout under a temp dir with a token file.
func NewDirs(t testing.TB, token string) Dirs {
	root := t.TempDir()
	d := Dirs{Root: root, Workspace: filepath.Join(root, "seat/workspace"), Home: filepath.Join(root, "seat/home"), Runner: filepath.Join(root, "seat/runner"), Tmp: filepath.Join(root, "tmp"), TokenFile: filepath.Join(root, "token")}
	for _, p := range []string{d.Workspace, d.Home, d.Runner, d.Tmp} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(d.TokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// Env builds an Environment for dirs against the tool server.
func Env(dirs Dirs, ts *ToolServer, toolCmd string) harnesses.Environment {
	return harnesses.Environment{
		OrganizationID: "org-1", SeatID: "seat-1", SeatKey: "conformance", DisplayName: "Conformance Seat",
		ConfigRevision: "rev-1", Generation: 3,
		PlatformURL: ts.URL, TokenFile: dirs.TokenFile,
		WorkspaceDir: dirs.Workspace, HomeDir: dirs.Home, RunnerDir: dirs.Runner, TmpDir: dirs.Tmp,
		Instructions: "Be helpful.",
		Bootstrap: runtimeapi.Bootstrap{
			Self:     runtimeapi.Self{SeatID: "seat-1", SeatKey: "conformance", DisplayName: "Conformance Seat", ConfigRevision: "rev-1"},
			Guidance: "Use memory tools to retrieve context.",
		},
		ToolCommand: toolCmd,
		ExtraEnv: []string{
			"STEADMESH_PLATFORM_URL=" + ts.URL,
			"STEADMESH_TOKEN_FILE=" + dirs.TokenFile,
			"STEADMESH_RUNNER_DIR=" + dirs.Runner,
			"STEADMESH_GENERATION=3",
		},
	}
}

func delivery(id, body string) harnesses.Delivery {
	return harnesses.Delivery{DeliveryID: 1, ExecutionID: "exec-" + id, Attempt: 1, Message: runtimeapi.Envelope{
		MessageID: id, ConversationID: "conv-1", Origin: "human", Binding: "alice", RecipientSeat: "conformance",
		Body: body, CreatedAt: time.Now().UTC(), ReplyRoute: "binding:alice",
	}}
}

// collector drains an adapter's events.
type collector struct {
	mu     sync.Mutex
	events []harnesses.Event
	done   chan struct{}
}

func collect(a harnesses.Adapter) *collector {
	c := &collector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for e := range a.Events() {
			c.mu.Lock()
			c.events = append(c.events, e)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) forExecution(id string) []harnesses.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []harnesses.Event
	for _, e := range c.events {
		if e.ExecutionID == id {
			out = append(out, e)
		}
	}
	return out
}

// waitFor polls until the collector has a completion for an execution; events
// are emitted before Deliver returns but delivered through a channel.
func (c *collector) waitCompletion(t *testing.T, id string) []harnesses.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := c.forExecution(id)
		for _, e := range evs {
			if e.Kind == harnesses.EventCompletion {
				return evs
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no completion event for %s; got %d events", id, len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fixture struct {
	opts  Options
	ts    *ToolServer
	tools string
	dirs  Dirs
}

func (f *fixture) env(t *testing.T) harnesses.Environment {
	env := Env(f.dirs, f.ts, f.tools)
	if f.opts.Configure != nil {
		f.opts.Configure(t, &env)
	}
	return env
}

func (f *fixture) start(t *testing.T, rec harnesses.RecoveryDescriptor) (harnesses.Adapter, *collector, harnesses.ResumeResult) {
	t.Helper()
	a := f.opts.New()
	c := collect(a)
	ctx := context.Background()
	if err := a.Prepare(ctx, f.env(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := a.StartOrResume(ctx, rec)
	if err != nil {
		t.Fatalf("StartOrResume: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, c, res
}

// Run executes the conformance suite.
func Run(t *testing.T, opts Options) {
	if opts.TurnTimeout == 0 {
		opts.TurnTimeout = 60 * time.Second
	}
	if opts.InterruptBound == 0 {
		opts.InterruptBound = 20 * time.Second
	}
	const token = "conformance-token"
	newFixture := func(t *testing.T) *fixture {
		return &fixture{opts: opts, ts: NewToolServer(t, token), tools: BuildTools(t), dirs: NewDirs(t, token)}
	}

	t.Run("DescribeCapabilities", func(t *testing.T) {
		caps := opts.New().DescribeCapabilities()
		if caps.Adapter == "" {
			t.Error("Adapter name is empty")
		}
		if len(caps.ToolTransports) == 0 {
			t.Error("no tool transports")
		}
		if !contains(caps.CheckpointGuarantees, harnesses.GuaranteeApplication) {
			t.Errorf("checkpoint guarantees %v lack %s", caps.CheckpointGuarantees, harnesses.GuaranteeApplication)
		}
		if !contains(caps.RecoveryModes, harnesses.RecoveryFresh) || !contains(caps.RecoveryModes, harnesses.RecoveryPortableHandoff) {
			t.Errorf("recovery modes %v must include fresh and portable_handoff", caps.RecoveryModes)
		}
		if caps.Interruption == "" {
			t.Error("Interruption mode not reported")
		}
	})

	t.Run("DeliverBeforeStartFails", func(t *testing.T) {
		f := newFixture(t)
		a := opts.New()
		_ = collect(a)
		if err := a.Prepare(context.Background(), f.env(t)); err != nil {
			t.Fatal(err)
		}
		defer a.Stop(context.Background())
		if _, err := a.Deliver(context.Background(), delivery("m0", opts.QuickBody)); err == nil {
			t.Fatal("Deliver before StartOrResume must fail")
		}
	})

	t.Run("TurnLifecycleAndRestart", func(t *testing.T) {
		f := newFixture(t)
		a, c, res := f.start(t, harnesses.RecoveryDescriptor{})
		if res.Mode != harnesses.RecoveryFresh {
			t.Fatalf("first start mode %q, want fresh", res.Mode)
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.TurnTimeout)
		defer cancel()
		d := delivery("m1", opts.QuickBody)
		tr, err := a.Deliver(ctx, d)
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if tr.Status != harnesses.TurnCompleted || tr.MessageID != "m1" {
			t.Fatalf("turn result %+v", tr)
		}
		evs := c.waitCompletion(t, d.ExecutionID)
		completions := 0
		for _, e := range evs {
			if e.MessageID != "m1" {
				t.Errorf("event %s has message id %q", e.Kind, e.MessageID)
			}
			if e.Time.IsZero() {
				t.Errorf("event %s has no time", e.Kind)
			}
			if !json.Valid(e.Data) && len(e.Data) > 0 {
				t.Errorf("event %s data is not JSON", e.Kind)
			}
			if e.Kind == harnesses.EventCompletion {
				completions++
			}
		}
		if completions != 1 || evs[len(evs)-1].Kind != harnesses.EventCompletion {
			t.Fatalf("want exactly one terminal completion event, got %v", kinds(evs))
		}
		cp, err := a.Checkpoint(context.Background())
		if err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		caps := a.DescribeCapabilities()
		if cp.HarnessAdapter != caps.Adapter || cp.FormatVersion == "" || cp.CheckpointRef == "" || !contains(caps.CheckpointGuarantees, cp.Guarantee) {
			t.Fatalf("checkpoint %+v inconsistent with capabilities %+v", cp, caps)
		}
		if err := a.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Fatal("Events channel not closed by Stop")
		}
		if _, err := a.Deliver(context.Background(), delivery("m2", opts.QuickBody)); err == nil {
			t.Fatal("Deliver after Stop must fail")
		}

		// Restart on the same volumes resumes the native session.
		b, _, res2 := f.start(t, harnesses.RecoveryDescriptor{Session: &cp})
		if res2.Mode != harnesses.RecoveryNativeResume {
			t.Fatalf("restart mode %q (%s), want native_resume", res2.Mode, res2.Note)
		}
		tr2, err := b.Deliver(ctx, delivery("m3", opts.QuickBody))
		if err != nil || tr2.Status != harnesses.TurnCompleted {
			t.Fatalf("turn after restart: %+v %v", tr2, err)
		}
	})

	t.Run("MissingSessionUsesPortableHandoff", func(t *testing.T) {
		f := newFixture(t)
		caps := opts.New().DescribeCapabilities()
		lost := runtimeapi.Checkpoint{HarnessAdapter: caps.Adapter, FormatVersion: caps.SessionFormat, CheckpointRef: "00000000-0000-4000-8000-000000000000", Guarantee: harnesses.GuaranteeApplication}
		a, _, res := f.start(t, harnesses.RecoveryDescriptor{Session: &lost, Handoff: &runtimeapi.Handoff{Objective: "finish the report"}})
		if res.Mode == harnesses.RecoveryNativeResume {
			// Adapters that detect a missing session lazily must report it on
			// the first turn instead.
			ctx, cancel := context.WithTimeout(context.Background(), opts.TurnTimeout)
			defer cancel()
			tr, err := a.Deliver(ctx, delivery("m1", opts.QuickBody))
			if err != nil || tr.Recovery == nil || tr.Recovery.Mode != harnesses.RecoveryPortableHandoff || tr.Recovery.Note == "" {
				t.Fatalf("missing native session claimed as resumed: start %+v turn %+v err %v", res, tr, err)
			}
			return
		}
		if res.Mode != harnesses.RecoveryPortableHandoff || res.Note == "" {
			t.Fatalf("mode %q note %q; want portable_handoff with a note", res.Mode, res.Note)
		}
	})

	t.Run("ForeignAdapterSessionUsesPortableHandoff", func(t *testing.T) {
		f := newFixture(t)
		foreign := runtimeapi.Checkpoint{HarnessAdapter: "some-other-harness", FormatVersion: "x/1", CheckpointRef: "abc", Guarantee: harnesses.GuaranteeApplication}
		_, _, res := f.start(t, harnesses.RecoveryDescriptor{Session: &foreign})
		if res.Mode != harnesses.RecoveryPortableHandoff || res.Note == "" {
			t.Fatalf("mode %q note %q; want portable_handoff with a note", res.Mode, res.Note)
		}
	})

	t.Run("InterruptByContext", func(t *testing.T) {
		f := newFixture(t)
		a, c, _ := f.start(t, harnesses.RecoveryDescriptor{})
		ctx, cancel := context.WithCancel(context.Background())
		d := delivery("slow1", opts.SlowBody)
		type out struct {
			tr  harnesses.TurnResult
			err error
		}
		ch := make(chan out, 1)
		go func() { tr, err := a.Deliver(ctx, d); ch <- out{tr, err} }()
		time.Sleep(1500 * time.Millisecond)
		cancel()
		select {
		case o := <-ch:
			if o.err != nil || o.tr.Status != harnesses.TurnInterrupted {
				t.Fatalf("interrupted turn: %+v %v", o.tr, o.err)
			}
		case <-time.After(opts.InterruptBound):
			t.Fatal("turn did not stop after interruption")
		}
		c.waitCompletion(t, d.ExecutionID)
		// The adapter remains usable.
		qctx, qcancel := context.WithTimeout(context.Background(), opts.TurnTimeout)
		defer qcancel()
		if tr, err := a.Deliver(qctx, delivery("after", opts.QuickBody)); err != nil || tr.Status != harnesses.TurnCompleted {
			t.Fatalf("turn after interrupt: %+v %v", tr, err)
		}
	})

	t.Run("QuiesceIdleThenRejects", func(t *testing.T) {
		f := newFixture(t)
		a, _, _ := f.start(t, harnesses.RecoveryDescriptor{})
		qr, err := a.Quiesce(context.Background())
		if err != nil || qr.Interrupted {
			t.Fatalf("idle quiesce: %+v %v", qr, err)
		}
		if _, err := a.Deliver(context.Background(), delivery("late", opts.QuickBody)); !errors.Is(err, harnesses.ErrQuiescing) {
			t.Fatalf("Deliver after Quiesce: %v, want ErrQuiescing", err)
		}
		if _, err := a.Checkpoint(context.Background()); err != nil {
			t.Fatalf("Checkpoint after Quiesce: %v", err)
		}
	})

	t.Run("QuiesceDeadlineInterrupts", func(t *testing.T) {
		f := newFixture(t)
		a, _, _ := f.start(t, harnesses.RecoveryDescriptor{})
		ch := make(chan harnesses.TurnResult, 1)
		go func() { tr, _ := a.Deliver(context.Background(), delivery("slow2", opts.SlowBody)); ch <- tr }()
		time.Sleep(1500 * time.Millisecond)
		qctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		qr, err := a.Quiesce(qctx)
		if err != nil || !qr.Interrupted {
			t.Fatalf("quiesce with active slow turn: %+v %v", qr, err)
		}
		if time.Since(start) > opts.InterruptBound {
			t.Fatalf("quiesce took %v", time.Since(start))
		}
		if tr := <-ch; tr.Status != harnesses.TurnInterrupted {
			t.Fatalf("turn status %q, want interrupted", tr.Status)
		}
	})

	t.Run("StopKillsActiveTurn", func(t *testing.T) {
		f := newFixture(t)
		a, c, _ := f.start(t, harnesses.RecoveryDescriptor{})
		ch := make(chan harnesses.TurnResult, 1)
		go func() { tr, _ := a.Deliver(context.Background(), delivery("slow3", opts.SlowBody)); ch <- tr }()
		time.Sleep(1500 * time.Millisecond)
		expired, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if err := a.Stop(expired); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		select {
		case tr := <-ch:
			if tr.Status != harnesses.TurnInterrupted {
				t.Fatalf("status %q", tr.Status)
			}
		case <-time.After(opts.InterruptBound):
			t.Fatal("active turn survived Stop")
		}
		if time.Since(start) > opts.InterruptBound {
			t.Fatalf("Stop took %v", time.Since(start))
		}
		<-c.done
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func kinds(evs []harnesses.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
