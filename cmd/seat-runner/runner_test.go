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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
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
	if r.Header.Get("Authorization") != "Bearer "+testToken {
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
	case strings.HasPrefix(path, runtimeapi.PathModelProxy) && strings.HasSuffix(path, "/v1/models"):
		p.record("model:" + path)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}]}`))
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
	r, err := NewRunner(cfg, log, DefaultAdapters)
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
	tr := newTestRunner(t, p, func(c *Config) { c.ModelConnection = "model" })
	p.waitFor(t, "state:Warm", 10*time.Second)
	d := p.enqueue("probe", "probe")
	p.waitFor(t, fmt.Sprintf("ack:%d:completed", d.DeliveryID), 10*time.Second)
	log := p.snapshot()
	if indexOf(log, "tool:self:"+d.ExecutionID) < 0 || indexOf(log, "model:/v1/model/model/v1/models") < 0 {
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
	if !pr.OK || pr.Checks["tool"] != "ok" || pr.Checks["workspace"] != "ok" || pr.Checks["model"] != "ok" {
		t.Fatalf("probe result %+v", pr)
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
