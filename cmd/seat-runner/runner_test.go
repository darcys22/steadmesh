package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/harnesses/fake"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/modelforward"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

const testToken = "seat-token"

// fakePlatform implements the seat-facing /v1 API in memory.
type fakePlatform struct {
	mu        sync.Mutex
	gen       int64
	heldFor   int  // number of acquire attempts to reject as held
	fence     bool // renewals fail with fenced
	queue     []runtimeapi.InboxDelivery
	log       []string
	events    map[string][]runtimeapi.ExecutionEvent
	acks      map[int64]runtimeapi.InboxAckRequest
	states    []runtimeapi.StateRequest
	nextDelID int64
}

func newFakePlatform() *fakePlatform {
	return &fakePlatform{events: map[string][]runtimeapi.ExecutionEvent{}, acks: map[int64]runtimeapi.InboxAckRequest{}}
}

func (p *fakePlatform) record(s string) {
	p.log = append(p.log, s)
}

func (p *fakePlatform) enqueue(origin, body string) runtimeapi.InboxDelivery {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextDelID++
	d := runtimeapi.InboxDelivery{DeliveryID: p.nextDelID, ExecutionID: fmt.Sprintf("exec-%d", p.nextDelID), Attempt: 1, Message: runtimeapi.Envelope{
		MessageID: fmt.Sprintf("msg-%d", p.nextDelID), ConversationID: "c1", Origin: origin, RecipientSeat: "alice", Body: body, Binding: "alice",
	}}
	p.queue = append(p.queue, d)
	return d
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(runtimeapi.Error{Code: code, Message: msg})
}

func (p *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a := r.Header.Get("Authorization"); a != "Bearer "+testToken && a != "Bearer rotated-token" {
		writeErr(w, 401, "unauthenticated", "bad token")
		return
	}
	path := r.URL.Path
	if path == runtimeapi.PathInboxNext {
		p.inboxNext(w, r)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mutating := r.Method != http.MethodGet && path != runtimeapi.PathLeaseAcquire && !strings.HasPrefix(path, runtimeapi.PathModelProxy)
	if mutating && !strings.HasPrefix(path, runtimeapi.PathToolCall) {
		if g, _ := strconv.ParseInt(r.Header.Get(runtimeapi.HeaderGeneration), 10, 64); g != p.gen {
			p.record("fenced:" + path)
			writeErr(w, 409, "fenced", fmt.Sprintf("generation %d != %d", g, p.gen))
			return
		}
	}
	switch {
	case path == runtimeapi.PathLeaseAcquire:
		if p.heldFor != 0 {
			if p.heldFor > 0 {
				p.heldFor--
			}
			p.record("acquire:held")
			writeErr(w, 409, "conflict", "lease held by another pod")
			return
		}
		p.gen++
		p.record(fmt.Sprintf("acquire:%d", p.gen))
		_ = json.NewEncoder(w).Encode(runtimeapi.LeaseResponse{SeatID: "seat-1", Generation: p.gen, ExpiresAt: time.Now().Add(30 * time.Second)})
	case path == runtimeapi.PathLeaseRenew:
		if p.fence {
			p.gen++ // another holder took over
			p.record("renew:fenced")
			writeErr(w, 409, "fenced", "lease taken over")
			return
		}
		_ = json.NewEncoder(w).Encode(runtimeapi.LeaseResponse{SeatID: "seat-1", Generation: p.gen})
	case path == runtimeapi.PathLeaseRelease:
		p.record("release")
		w.WriteHeader(204)
	case path == runtimeapi.PathState:
		var s runtimeapi.StateRequest
		_ = json.NewDecoder(r.Body).Decode(&s)
		p.states = append(p.states, s)
		p.record("state:" + s.State)
		w.WriteHeader(204)
	case path == runtimeapi.PathBootstrap:
		_ = json.NewEncoder(w).Encode(runtimeapi.Bootstrap{Self: runtimeapi.Self{SeatID: "seat-1", SeatKey: "alice", ConfigRevision: "rev-1"}, Instructions: "be good", Guidance: "use memory"})
	case path == runtimeapi.PathTools:
		_ = json.NewEncoder(w).Encode(runtimeapi.ToolList{Tools: conformance.Tools})
	case strings.HasPrefix(path, runtimeapi.PathToolCall):
		name := strings.TrimPrefix(path, runtimeapi.PathToolCall)
		p.record("tool:" + name + ":" + r.Header.Get(runtimeapi.HeaderExecution))
		_, _ = w.Write([]byte(`{"content":{"ok":true}}`))
	case strings.HasPrefix(path, runtimeapi.PathInboxAck):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, runtimeapi.PathInboxAck), "/ack"), 10, 64)
		var a runtimeapi.InboxAckRequest
		_ = json.NewDecoder(r.Body).Decode(&a)
		p.acks[id] = a
		p.record(fmt.Sprintf("ack:%d:%s", id, a.Outcome))
		w.WriteHeader(204)
	case strings.HasPrefix(path, runtimeapi.PathExecEvents):
		id := strings.TrimSuffix(strings.TrimPrefix(path, runtimeapi.PathExecEvents), "/events")
		var b []runtimeapi.ExecutionEvent
		_ = json.NewDecoder(r.Body).Decode(&b)
		p.events[id] = append(p.events[id], b...)
		w.WriteHeader(204)
	case path == runtimeapi.PathCheckpoint:
		var c runtimeapi.Checkpoint
		_ = json.NewDecoder(r.Body).Decode(&c)
		p.record("checkpoint:" + c.CheckpointRef)
		w.WriteHeader(204)
	case strings.HasPrefix(path, runtimeapi.PathModelProxy):
		p.record(fmt.Sprintf("model:%s %s auth=%s gen=%s exec=%s", r.Method, path, r.Header.Get("Authorization"),
			r.Header.Get(runtimeapi.HeaderGeneration), r.Header.Get(runtimeapi.HeaderExecution)))
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		writeErr(w, 404, "not_found", path)
	}
}

func (p *fakePlatform) inboxNext(w http.ResponseWriter, r *http.Request) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			d := p.queue[0]
			p.queue = p.queue[1:]
			p.record(fmt.Sprintf("leased:%d", d.DeliveryID))
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(d)
			return
		}
		p.mu.Unlock()
		if time.Now().After(deadline) || r.Context().Err() != nil {
			w.WriteHeader(204)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *fakePlatform) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.log...)
}

func (p *fakePlatform) waitFor(t *testing.T, entry string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if indexOf(p.snapshot(), entry) >= 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; log: %v", entry, p.snapshot())
}

func indexOf(log []string, prefix string) int {
	for i, l := range log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func lastIndexOf(log []string, prefix string) int {
	for i := len(log) - 1; i >= 0; i-- {
		if strings.HasPrefix(log[i], prefix) {
			return i
		}
	}
	return -1
}

type testRunner struct {
	r    *Runner
	p    *fakePlatform
	stop context.CancelFunc
	done chan error
	cfg  Config
}

func newTestRunner(t *testing.T, p *fakePlatform, mutate func(*Config)) *testRunner {
	t.Helper()
	return newTestRunnerWith(t, p, mutate, DefaultAdapters)
}

func newTestRunnerWith(t *testing.T, p *fakePlatform, mutate func(*Config), adapters AdapterFactory) *testRunner {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	tf := filepath.Join(root, "token")
	_ = os.WriteFile(tf, []byte(testToken), 0o600)
	cfg := Config{
		PlatformURL: srv.URL, TokenFile: tf, OrgID: "org-1", SeatID: "seat-1", SeatKey: "alice", ConfigRevision: "rev-1", Harness: "fake",
		ManifestDir: filepath.Join(root, "manifest"), PodUID: "pod-1",
		WorkspaceDir: filepath.Join(root, "seat/workspace"), HomeDir: filepath.Join(root, "seat/home"), RunnerDir: filepath.Join(root, "seat/runner"), TmpDir: filepath.Join(root, "tmp"),
		ToolCommand:         conformance.BuildTools(t),
		LeaseAcquireTimeout: 5 * time.Second, LeaseRetryInterval: 50 * time.Millisecond, RenewInterval: 100 * time.Millisecond, LeaseTTL: time.Second,
		PollWait: time.Second, TurnTimeout: time.Minute, QuiesceTimeout: 10 * time.Second, BootstrapTimeout: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	r, err := NewRunner(cfg, log, adapters)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &testRunner{r: r, p: p, stop: cancel, done: make(chan error, 1), cfg: cfg}
	go func() { tr.done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tr.done:
		case <-time.After(15 * time.Second):
		}
	})
	return tr
}

func (tr *testRunner) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-tr.done:
		tr.done <- err
		return err
	case <-time.After(timeout):
		t.Fatalf("runner did not exit; log: %v", tr.p.snapshot())
		return nil
	}
}

func TestDeliveryAndSIGTERMQuiesceOrdering(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, nil)
	p.waitFor(t, "state:Warm", 10*time.Second)
	if !tr.r.Ready() {
		t.Fatal("runner should be ready after lease and bootstrap")
	}
	d := p.enqueue("human", "/sleep 800ms\n/reply hi")
	p.waitFor(t, "state:Executing", 5*time.Second)
	tr.stop() // SIGTERM while the turn is running
	if err := tr.wait(t, 15*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := p.snapshot()
	order := []int{
		indexOf(log, "state:Executing"),
		indexOf(log, "state:Quiescing"),
		indexOf(log, "tool:messages.reply:"+d.ExecutionID),
		indexOf(log, fmt.Sprintf("ack:%d:completed", d.DeliveryID)),
		lastIndexOf(log, "checkpoint:"),
		indexOf(log, "state:Stopped"),
		indexOf(log, "release"),
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] < 0 || order[i-1] >= order[i] {
			t.Fatalf("bad ordering %v in log %v", order, log)
		}
	}
	if indexOf(log[order[1]:], "state:Warm") >= 0 {
		t.Fatalf("Warm reported after Quiescing: %v", log)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states[0].State != StateWarm || p.states[0].AdoptedRevision != "rev-1" || p.states[0].Generation != 1 {
		t.Fatalf("first state %+v", p.states[0])
	}
	kinds := map[string]int{}
	for _, e := range p.events[d.ExecutionID] {
		kinds[e.Kind]++
	}
	if kinds["completion"] != 1 || kinds["tool_request"] != 1 || kinds["tool_result"] != 1 {
		t.Fatalf("events %v", kinds)
	}
	if b, _ := os.ReadFile(filepath.Join(tr.cfg.RunnerDir, "generation")); strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("generation file %q", b)
	}
}

func TestLeaseAcquireRetriesWhileHeld(t *testing.T) {
	p := newFakePlatform()
	p.heldFor = 3
	tr := newTestRunner(t, p, nil)
	p.waitFor(t, "state:Warm", 10*time.Second)
	log := p.snapshot()
	held := 0
	for _, l := range log {
		if l == "acquire:held" {
			held++
		}
	}
	if held != 3 || indexOf(log, "acquire:1") < 0 {
		t.Fatalf("log %v", log)
	}
	tr.stop()
	if err := tr.wait(t, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseAcquireTimeout(t *testing.T) {
	p := newFakePlatform()
	p.heldFor = -1 // forever
	tr := newTestRunner(t, p, func(c *Config) { c.LeaseAcquireTimeout = 300 * time.Millisecond })
	err := tr.wait(t, 10*time.Second)
	if !errors.Is(err, ErrLeaseTimeout) || ExitCode(err) != exitLeaseTimeout {
		t.Fatalf("err %v code %d", err, ExitCode(err))
	}
	if tr.r.Ready() {
		t.Fatal("ready without a lease")
	}
}

func TestFencedRenewalKillsHarness(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, nil)
	p.waitFor(t, "state:Warm", 10*time.Second)
	d := p.enqueue("human", "/sleep 30s\n/reply never")
	p.waitFor(t, "state:Executing", 5*time.Second)
	start := time.Now()
	p.mu.Lock()
	p.fence = true
	p.mu.Unlock()
	err := tr.wait(t, 5*time.Second)
	if !errors.Is(err, ErrFenced) || ExitCode(err) != exitFenced {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("fencing took %v", time.Since(start))
	}
	log := p.snapshot()
	for _, bad := range []string{fmt.Sprintf("ack:%d", d.DeliveryID), "tool:messages.reply", "release", "state:Stopped"} {
		if indexOf(log, bad) >= 0 {
			t.Fatalf("stale writer continued (%s): %v", bad, log)
		}
	}
	if tr.r.Ready() {
		t.Fatal("ready after fencing")
	}
}

func TestProbeHandledByRunner(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, nil)
	p.waitFor(t, "state:Warm", 10*time.Second)
	d := p.enqueue("probe", "probe")
	p.waitFor(t, fmt.Sprintf("ack:%d:completed", d.DeliveryID), 10*time.Second)
	log := p.snapshot()
	if indexOf(log, "tool:self:"+d.ExecutionID) < 0 {
		t.Fatalf("probe checks not performed: %v", log)
	}
	if indexOf(log, "state:Executing") >= 0 {
		t.Fatalf("probe must not reach the harness: %v", log)
	}
	p.mu.Lock()
	evs := p.events[d.ExecutionID]
	p.mu.Unlock()
	if len(evs) != 1 || evs[0].Kind != runtimeapi.EventProbeResult {
		t.Fatalf("events %+v", evs)
	}
	var pr runtimeapi.ProbeResult
	_ = json.Unmarshal(evs[0].Data, &pr)
	if !pr.OK || pr.Checks["tool"] != "ok" || pr.Checks["workspace"] != "ok" {
		t.Fatalf("probe result %+v", pr)
	}
	if _, ok := pr.Checks["model"]; ok {
		t.Fatalf("a seat without a model has no model check: %+v", pr)
	}
	entries, _ := os.ReadDir(filepath.Join(tr.cfg.RunnerDir, "probe"))
	if len(entries) != 0 {
		t.Fatalf("probe left files: %v", entries)
	}
}

func TestQuiesceBoundInterruptsTurn(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, func(c *Config) { c.QuiesceTimeout = 300 * time.Millisecond })
	p.waitFor(t, "state:Warm", 10*time.Second)
	d := p.enqueue("human", "/sleep 30s\n/reply never")
	p.waitFor(t, "state:Executing", 5*time.Second)
	start := time.Now()
	tr.stop()
	if err := tr.wait(t, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("quiesce took %v", time.Since(start))
	}
	log := p.snapshot()
	if indexOf(log, fmt.Sprintf("ack:%d", d.DeliveryID)) >= 0 {
		t.Fatalf("interrupted turn must be left for redelivery: %v", log)
	}
	if !(indexOf(log, "state:Quiescing") < lastIndexOf(log, "checkpoint:") && lastIndexOf(log, "checkpoint:") < indexOf(log, "state:Stopped") && indexOf(log, "state:Stopped") < indexOf(log, "release")) {
		t.Fatalf("ordering: %v", log)
	}
}

func TestTurnTimeoutAcksFailed(t *testing.T) {
	p := newFakePlatform()
	_ = newTestRunner(t, p, func(c *Config) { c.TurnTimeout = 300 * time.Millisecond })
	p.waitFor(t, "state:Warm", 10*time.Second)
	d := p.enqueue("human", "/sleep 30s")
	p.waitFor(t, fmt.Sprintf("ack:%d:failed", d.DeliveryID), 10*time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.Contains(p.acks[d.DeliveryID].Error, "timed_out") {
		t.Fatalf("ack %+v", p.acks[d.DeliveryID])
	}
}

func TestReadiness(t *testing.T) {
	p := newFakePlatform()
	p.heldFor = -1
	tr := newTestRunner(t, p, func(c *Config) { c.LeaseAcquireTimeout = 2 * time.Second })
	h := tr.r.Handler()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s = %d, want %d", path, rec.Code, want)
		}
	}
	p.mu.Lock()
	p.heldFor = 0
	p.mu.Unlock()
	p.waitFor(t, "state:Warm", 10*time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("readyz after lease = %d", rec.Code)
	}
}

func TestCapData(t *testing.T) {
	small := json.RawMessage(`{"a":1}`)
	if string(capData(small)) != string(small) {
		t.Fatal("small payload changed")
	}
	big, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", runtimeapi.MaxEventDataBytes)})
	c := capData(big)
	var m map[string]any
	if err := json.Unmarshal(c, &m); err != nil || m["truncated"] != true || len(c) > runtimeapi.MaxEventDataBytes {
		t.Fatalf("capped %d bytes: %v", len(c), err)
	}
}

func TestNetprobe(t *testing.T) {
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer allowed.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := "http://" + ln.Addr().String()
	ln.Close()
	term := filepath.Join(t.TempDir(), "termination-log")
	var out strings.Builder
	if code := netprobeMain([]string{"--allowed", allowed.URL, "--denied", closed, "--timeout", "1s"}, &out, io.Discard, term); code != 0 {
		t.Fatalf("enforced case exit %d: %s", code, out.String())
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "enforced=true") {
		t.Fatalf("termination log %q", b)
	}
	if code := netprobeMain([]string{"--allowed", closed, "--denied", allowed.URL, "--timeout", "1s"}, io.Discard, io.Discard, term); code != 1 {
		t.Fatalf("unenforced case exit %d", code)
	}
	if b, _ := os.ReadFile(term); !strings.Contains(string(b), "enforced=false") {
		t.Fatalf("termination log %q", b)
	}
	if code := netprobeMain([]string{"--allowed", allowed.URL}, io.Discard, io.Discard, ""); code != 2 {
		t.Fatalf("usage exit %d", code)
	}
}

func TestProbeFailureConvention(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, nil)
	p.waitFor(t, "state:Warm", 10*time.Second)
	tr.r.cfg.ToolCommand = filepath.Join(t.TempDir(), "missing-steadmesh-tools")
	d := p.enqueue("probe", "probe")
	// The verdict is in data.checks; the delivery is still acked completed.
	p.waitFor(t, fmt.Sprintf("ack:%d:completed", d.DeliveryID), 10*time.Second)
	p.mu.Lock()
	evs := p.events[d.ExecutionID]
	p.mu.Unlock()
	var data struct {
		Checks map[string]string `json:"checks"`
	}
	_ = json.Unmarshal(evs[0].Data, &data)
	if !strings.HasPrefix(data.Checks["tool"], "fail") || data.Checks["workspace"] != "ok" {
		t.Fatalf("checks %+v", data.Checks)
	}
}

func TestCapDataEscapeHeavy(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"text": strings.Repeat("\x01\"<", runtimeapi.MaxEventDataBytes)})
	if c := capData(big); len(c) > runtimeapi.MaxEventDataBytes || !json.Valid(c) {
		t.Fatalf("capped to %d bytes", len(c))
	}
}

// modelHarness is a minimal model-backed adapter: each turn sends one
// request to its model endpoint and, when told to, reports a self tool call.
type modelHarness struct {
	*fake.Adapter
	env      harnesses.Environment
	callSelf bool
}

func (m *modelHarness) Prepare(ctx context.Context, env harnesses.Environment) error {
	m.env = env
	return m.Adapter.Prepare(ctx, env)
}

func (m *modelHarness) Deliver(ctx context.Context, d harnesses.Delivery) (harnesses.TurnResult, error) {
	body := fmt.Sprintf(`{"model":%q}`, m.env.Model.ID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.env.Model.BaseURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", m.env.Model.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return harnesses.TurnResult{MessageID: d.Message.MessageID, Status: harnesses.TurnFailed, Error: err.Error()}, nil
	}
	resp.Body.Close()
	if m.callSelf {
		d.Message.Body = "/tool self {}"
	} else {
		d.Message.Body = "nothing"
	}
	return m.Adapter.Deliver(ctx, d)
}

func writeModelManifest(t *testing.T, dir string) {
	t.Helper()
	sm := compile.SeatManifest{Key: "alice", Harness: spec.HarnessProfile{Adapter: "model-harness",
		Model: &spec.ModelSelection{Connection: "llm", ID: "m-1", API: harnesses.APIAnthropicMessages}}}
	b, _ := json.Marshal(sm)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModelHarnessProbeAndForwarder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		callSelf bool
		wantOK   bool
	}{{"proves the harness", true, true}, {"no self call fails the tool check", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakePlatform()
			var mh *modelHarness
			factory := func(string) (harnesses.Adapter, error) {
				mh = &modelHarness{Adapter: fake.New(), callSelf: tc.callSelf}
				return mh, nil
			}
			tr := newTestRunnerWith(t, p, func(c *Config) {
				c.Harness = "model-harness"
				writeModelManifest(t, c.ManifestDir)
			}, factory)
			p.waitFor(t, "state:Warm", 10*time.Second)
			if mh.env.Model == nil || mh.env.Model.ID != "m-1" || !strings.HasPrefix(mh.env.Model.BaseURL, "http://127.0.0.1:") || mh.env.Model.APIKey != modelforward.LocalAPIKey {
				t.Fatalf("model environment %+v", mh.env.Model)
			}
			d := p.enqueue("probe", "probe")
			p.waitFor(t, fmt.Sprintf("ack:%d:completed", d.DeliveryID), 10*time.Second)
			// The forwarder replaced the placeholder with the seat token and
			// added the generation and execution.
			want := fmt.Sprintf("model:POST %sllm/v1/messages auth=Bearer %s gen=", runtimeapi.PathModelProxy, testToken)
			log := p.snapshot()
			i := slices.IndexFunc(log, func(s string) bool { return strings.HasPrefix(s, want) })
			if i < 0 || !strings.HasSuffix(log[i], "exec="+d.ExecutionID) {
				t.Fatalf("forwarded request not found (want %q): %v", want, log)
			}
			p.mu.Lock()
			evs := p.events[d.ExecutionID]
			p.mu.Unlock()
			var pr runtimeapi.ProbeResult
			for _, e := range evs {
				if e.Kind == runtimeapi.EventProbeResult {
					_ = json.Unmarshal(e.Data, &pr)
				}
			}
			if pr.OK != tc.wantOK || pr.Checks["model"] != "ok" || pr.Checks["turn"] != "ok" || (pr.Checks["tool"] == "ok") != tc.wantOK {
				t.Fatalf("probe result %+v", pr)
			}

			// A rotated token is used by the next request without a restart.
			if err := os.WriteFile(tr.cfg.TokenFile, []byte("rotated-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			d2 := p.enqueue("human", "hello")
			p.waitFor(t, fmt.Sprintf("ack:%d:completed", d2.DeliveryID), 10*time.Second)
			if slices.IndexFunc(p.snapshot(), func(s string) bool { return strings.Contains(s, "auth=Bearer rotated-token") }) < 0 {
				t.Fatalf("rotated token not forwarded: %v", p.snapshot())
			}
		})
	}
}

func writeAccessManifest(t *testing.T, dir string, a *access.SeatAccess) {
	t.Helper()
	b, _ := json.Marshal(compile.SeatManifest{Key: "alice", DisplayName: "Alice", Harness: spec.HarnessProfile{Adapter: "fake"}, Access: a})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingBinaryBlocksStart(t *testing.T) {
	p := newFakePlatform()
	tr := newTestRunner(t, p, func(c *Config) {
		writeAccessManifest(t, c.ManifestDir, &access.SeatAccess{Profiles: []string{"tools"}, Binaries: []string{"steadmesh-no-such-binary"}})
	})
	err := tr.wait(t, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "need steadmesh-no-such-binary") {
		t.Fatalf("runner error %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	last := p.states[len(p.states)-1]
	if last.State != "Stopped" || !strings.Contains(last.Detail, "access profiles [tools] need steadmesh-no-such-binary") {
		t.Fatalf("reported state %+v", last)
	}
}

func TestAccessEnvironment(t *testing.T) {
	p := newFakePlatform()
	var env harnesses.Environment
	factory := func(string) (harnesses.Adapter, error) {
		return &envCapture{Adapter: fake.New(), env: &env}, nil
	}
	newTestRunnerWith(t, p, func(c *Config) {
		c.EgressURL = "http://egress.test:3128"
		writeAccessManifest(t, c.ManifestDir, &access.SeatAccess{Profiles: []string{"gh"},
			Egress: []access.EgressRule{{Host: "github.com", Ports: []int{443}}},
			GitHub: []access.GitHubGrant{{Connection: "github", Delivery: access.DeliverySandbox, Host: "github.com"}}})
	}, factory)
	p.waitFor(t, "state:Warm", 10*time.Second)
	got := strings.Join(env.SandboxEnv, "\n")
	for _, want := range []string{"HTTPS_PROXY=http://127.0.0.1:", "NO_PROXY=127.0.0.1,localhost,::1,127.0.0.1",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=\n", "credential git", "GIT_CONFIG_VALUE_2=alice@seats.steadmesh.invalid", "GIT_CONFIG_VALUE_3=Alice", "GIT_CONFIG_COUNT=4"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("sandbox env lacks %q:\n%s", want, got)
		}
	}
}

// envCapture records the environment it is prepared with.
type envCapture struct {
	*fake.Adapter
	env *harnesses.Environment
}

func (e *envCapture) Prepare(ctx context.Context, env harnesses.Environment) error {
	*e.env = env
	return e.Adapter.Prepare(ctx, env)
}
