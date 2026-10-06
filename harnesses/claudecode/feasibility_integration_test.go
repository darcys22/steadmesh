//go:build integration

// Feasibility spike (design §7, M1 exit evidence): drives the locally
// installed `claude` CLI through the real adapter against a stub
// Anthropic-compatible server and a stub platform tool API. It never uses the
// developer's login: HOME is a temp dir, --bare skips keychain/OAuth, and the
// only credential is a fake seat token served by the stub.
//
//	go test -tags integration -run Feasibility -v ./harnesses/claudecode/
//
// Set CLAUDE_FEASIBILITY_RECORD=<dir> to save raw stream-json lines.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

const seatToken = "seat-token-feasibility"

// acceptedTokens are the seat tokens the stub model proxy accepts.
var acceptedTokens = map[string]bool{seatToken: true, "rotated": true, "conformance-token": true}

// modelRequest is what the stub model records per request.
type modelRequest struct {
	Method, Path  string
	Authorization string
	XAPIKey       string
	Generation    string
	Execution     string
	Stream        bool
	Messages      int
	Tools         []string
	SystemHasSeat bool
	FirstUserText string
	HasToolResult bool
}

type stubModel struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []modelRequest
}

func newStubModel(t *testing.T) *stubModel {
	s := &stubModel{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *stubModel) requests() []modelRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]modelRequest(nil), s.reqs...)
}

func (s *stubModel) messagesRequests() []modelRequest {
	var out []modelRequest
	for _, r := range s.requests() {
		if strings.HasSuffix(r.Path, "/v1/messages") {
			out = append(out, r)
		}
	}
	return out
}

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if tx, ok := m["text"].(string); ok {
					sb.WriteString(tx)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func (s *stubModel) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		System any `json:"system"`
	}
	_ = json.Unmarshal(body, &req)
	rec := modelRequest{
		Method: r.Method, Path: r.URL.Path,
		Authorization: r.Header.Get("Authorization"), XAPIKey: r.Header.Get("X-Api-Key"),
		Generation: r.Header.Get(runtimeapi.HeaderGeneration), Execution: r.Header.Get(runtimeapi.HeaderExecution),
		Stream: req.Stream, Messages: len(req.Messages),
	}
	for _, tl := range req.Tools {
		rec.Tools = append(rec.Tools, tl.Name)
	}
	sys, _ := json.Marshal(req.System)
	rec.SystemHasSeat = strings.Contains(string(sys), "Seat identity")
	if len(req.Messages) > 0 {
		rec.FirstUserText = textOf(req.Messages[0].Content)
		last := req.Messages[len(req.Messages)-1]
		if arr, ok := last.Content.([]any); ok {
			for _, b := range arr {
				if m, ok := b.(map[string]any); ok && m["type"] == "tool_result" {
					rec.HasToolResult = true
				}
			}
		}
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	n := len(s.reqs)
	s.mu.Unlock()
	if dir := os.Getenv("CLAUDE_FEASIBILITY_RECORD"); dir != "" && len(body) > 0 {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("model-request-%02d.json", n)), body, 0o644)
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/count_tokens"):
		_, _ = w.Write([]byte(`{"input_tokens":10}`))
		return
	case !strings.HasSuffix(r.URL.Path, "/v1/messages"):
		w.WriteHeader(http.StatusOK)
		return
	}
	tok := strings.TrimPrefix(rec.Authorization, "Bearer ")
	if tok == "" {
		tok = rec.XAPIKey
	}
	if !acceptedTokens[tok] {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad seat token"}}`))
		return
	}
	lastText := ""
	if n := len(req.Messages); n > 0 {
		lastText = textOf(req.Messages[n-1].Content)
	}
	hasTool := false
	for _, n := range rec.Tools {
		if n == "mcp__steadmesh__memory_write" {
			hasTool = true
		}
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ns","type":"message","role":"assistant","model":"stub","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	ev := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if fl != nil {
			fl.Flush()
		}
	}
	ev("message_start", `{"type":"message_start","message":{"id":"`+fmt.Sprintf("msg_stub_%d", n)+`","type":"message","role":"assistant","model":"stub","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`)
	switch {
	case rec.HasToolResult:
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"memory written; replied via tool"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":6}}`)
	case strings.Contains(lastText, "SLOW"):
		// Hold the stream open until the client goes away (interrupt test).
		<-r.Context().Done()
		return
	case strings.Contains(lastText, "WRITE") && hasTool:
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_stub_1","name":"mcp__steadmesh__memory_write","input":{}}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"store\":\"rep_alice\",\"title\":\"t\",\"body\":\"b\"}"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`)
	default:
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from stub"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`)
	}
	ev("message_stop", `{"type":"message_stop"}`)
}

func requireClaude(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not installed")
	}
	return p
}

type spike struct {
	model *stubModel
	tools *conformance.ToolServer
	dirs  conformance.Dirs
	env   harnesses.Environment
}

func newSpike(t *testing.T, cfg map[string]string) *spike {
	requireClaude(t)
	s := &spike{model: newStubModel(t), tools: conformance.NewToolServer(t, seatToken), dirs: conformance.NewDirs(t, seatToken)}
	s.env = conformance.Env(s.dirs, s.tools, conformance.BuildTools(t))
	s.env.ModelProxyURL = s.model.URL + "/v1/model/model"
	s.env.ModelConnection = "model"
	s.env.Model = "claude-sonnet-4-5"
	s.env.HarnessConfig = map[string]string{ConfigInterruptGrace: "5s"}
	if v := os.Getenv("CLAUDE_FEASIBILITY_BARE"); v != "" {
		// Only set false inside a container: off --bare, Claude Code may read
		// the host keychain on macOS.
		s.env.HarnessConfig[ConfigBare] = v
	}
	for k, v := range cfg {
		s.env.HarnessConfig[k] = v
	}
	return s
}

func (s *spike) start(t *testing.T, rec harnesses.RecoveryDescriptor, tap func([]byte)) (*Adapter, func() []harnesses.Event, harnesses.ResumeResult) {
	t.Helper()
	a := New()
	a.Tap = tap
	var mu sync.Mutex
	var all []harnesses.Event
	go func() {
		for e := range a.Events() {
			mu.Lock()
			all = append(all, e)
			mu.Unlock()
		}
	}()
	// evs waits until the last delivered turn's completion event has been
	// received from the channel, then returns a snapshot.
	evs := func() []harnesses.Event {
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			snap := append([]harnesses.Event(nil), all...)
			mu.Unlock()
			if n := len(snap); (n > 0 && snap[n-1].Kind == harnesses.EventCompletion) || time.Now().After(deadline) {
				return snap
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	ctx := context.Background()
	if err := a.Prepare(ctx, s.env); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := a.StartOrResume(ctx, rec)
	if err != nil {
		t.Fatalf("StartOrResume: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, evs, res
}

func msg(id, body string) harnesses.Delivery {
	return harnesses.Delivery{DeliveryID: 1, ExecutionID: "exec-" + id, Attempt: 1, Message: runtimeapi.Envelope{
		MessageID: id, ConversationID: "conv-1", Origin: "human", Binding: "alice", RecipientSeat: "conformance", Body: body, ReplyRoute: "binding:alice",
	}}
}

func recorder(t *testing.T, name string) func([]byte) {
	dir := os.Getenv("CLAUDE_FEASIBILITY_RECORD")
	if dir == "" {
		return func([]byte) {}
	}
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.Create(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var mu sync.Mutex
	return func(l []byte) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = f.Write(append(l, '\n'))
	}
}

func TestFeasibilityVersionAndFlags(t *testing.T) {
	bin := requireClaude(t)
	home := t.TempDir()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claude --version: %s", strings.TrimSpace(string(out)))
	if v := versionRe.FindString(string(out)); v != PinnedVersion {
		// The seat image pins PinnedVersion; a host CLI that has auto-updated
		// is still worth checking for the flags the adapter relies on.
		t.Logf("note: host claude %s differs from the version pinned in the seat image (%s); checking flags against the host binary", v, PinnedVersion)
	}
	cmd := exec.Command(bin, "--help")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	help, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--print", "--output-format", "--verbose", "--resume", "--mcp-config", "--strict-mcp-config", "--append-system-prompt", "--permission-mode", "bypassPermissions", "--permission-prompts", "--settings", "--model", "--bare", "--system-prompt-snapshot", "--allowedTools"} {
		if !strings.Contains(string(help), flag) {
			t.Errorf("claude --help lacks %s", flag)
		}
	}
}

func TestFeasibilityEndToEnd(t *testing.T) {
	s := newSpike(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Turn 1: unattended start, MCP registration and a tool call via steadmesh-tools.
	a, evs, res := s.start(t, harnesses.RecoveryDescriptor{}, recorder(t, "turn1_tool_call"))
	if res.Mode != harnesses.RecoveryFresh {
		t.Fatalf("mode %q", res.Mode)
	}
	tr, err := a.Deliver(ctx, msg("m1", "Please WRITE a memory."))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 1: %+v", tr)
	if tr.Status != harnesses.TurnCompleted || tr.SessionID == "" {
		t.Fatalf("turn 1 result %+v", tr)
	}
	calls := s.tools.Calls()
	if len(calls) != 1 || calls[0].Name != "memory.write" {
		t.Fatalf("tool calls %+v", calls)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	if args["store"] != "rep_alice" || calls[0].Generation != "3" {
		t.Fatalf("tool call %+v args %v", calls[0], args)
	}
	t.Logf("tool call through steadmesh-tools mcp: %s %s generation=%s execution=%s", calls[0].Name, calls[0].Arguments, calls[0].Generation, calls[0].Execution)
	kinds := map[string]int{}
	var initData map[string]any
	for _, e := range evs() {
		kinds[e.Kind]++
		var d map[string]any
		_ = json.Unmarshal(e.Data, &d)
		if d["phase"] == "init" {
			initData = d
		}
	}
	t.Logf("event kinds turn 1: %v", kinds)
	t.Logf("init: %v", initData)
	if kinds[harnesses.EventToolRequest] < 1 || kinds[harnesses.EventToolResult] < 1 || kinds[harnesses.EventCompletion] != 1 {
		t.Fatalf("missing events: %v", kinds)
	}
	if kinds[harnesses.EventError] != 0 {
		t.Errorf("unexpected error events: %v", evs())
	}
	if initData["api_key_source"] != "apiKeyHelper" {
		t.Errorf("api key source %v", initData["api_key_source"])
	}
	mreqs := s.model.messagesRequests()
	if len(mreqs) < 2 {
		t.Fatalf("model requests %+v", mreqs)
	}
	for _, r := range mreqs {
		t.Logf("model request: %s auth=%q x-api-key=%q gen=%q exec=%q stream=%v msgs=%d tools=%d system_has_seat=%v", r.Path, r.Authorization, r.XAPIKey, r.Generation, r.Execution, r.Stream, r.Messages, len(r.Tools), r.SystemHasSeat)
		if r.Path != "/v1/model/model/v1/messages" || r.Generation != "3" || r.Execution != "exec-m1" || !r.SystemHasSeat {
			t.Errorf("model request %+v", r)
		}
	}
	for _, r := range s.model.requests() {
		if !strings.HasSuffix(r.Path, "/v1/messages") {
			t.Logf("other model-proxy request: %s %s", r.Method, r.Path)
		}
	}

	// Turn 2: --resume continues the session (history is sent again).
	before := len(s.model.messagesRequests())
	tr2, err := a.Deliver(ctx, msg("m2", "Just say hello."))
	if err != nil || tr2.Status != harnesses.TurnCompleted {
		t.Fatalf("turn 2 %+v %v", tr2, err)
	}
	if tr2.SessionID != tr.SessionID {
		t.Fatalf("resume changed session %s -> %s", tr.SessionID, tr2.SessionID)
	}
	r2 := s.model.messagesRequests()[before]
	if r2.Messages <= 1 || !strings.Contains(r2.FirstUserText, "WRITE") {
		t.Fatalf("resumed request lacks history: %+v", r2)
	}
	t.Logf("turn 2 resumed session %s with %d messages of history", tr2.SessionID, r2.Messages)

	// Restart: a new adapter on the same volumes resumes natively.
	cp, err := a.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint: %+v", cp)
	_ = a.Stop(ctx)
	b, _, res3 := s.start(t, harnesses.RecoveryDescriptor{Session: &cp}, nil)
	if res3.Mode != harnesses.RecoveryNativeResume || res3.SessionID != tr.SessionID {
		t.Fatalf("restart %+v", res3)
	}
	before = len(s.model.messagesRequests())
	tr3, err := b.Deliver(ctx, msg("m3", "Hello again."))
	if err != nil || tr3.Status != harnesses.TurnCompleted || tr3.SessionID != tr.SessionID {
		t.Fatalf("turn 3 %+v %v", tr3, err)
	}
	if r3 := s.model.messagesRequests()[before]; r3.Messages < 7 {
		t.Fatalf("restart did not carry history: %+v", r3)
	}
}

func TestFeasibilityResumeFallback(t *testing.T) {
	s := newSpike(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, evs, _ := s.start(t, harnesses.RecoveryDescriptor{}, recorder(t, "resume_missing"))
	// Simulate a session id whose transcript is gone (e.g. HOME lost) after start.
	a.mu.Lock()
	a.session.SessionID = "11111111-1111-4111-8111-111111111111"
	a.mu.Unlock()
	tr, err := a.Deliver(ctx, msg("m1", "Hello."))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fallback turn: %+v recovery=%+v", tr, tr.Recovery)
	if tr.Status != harnesses.TurnCompleted || tr.Recovery == nil || tr.Recovery.Mode != harnesses.RecoveryPortableHandoff || tr.SessionID == "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("expected portable handoff fallback, got %+v", tr)
	}
	found := false
	for _, e := range evs() {
		if strings.Contains(string(e.Data), "resume_failed") {
			found = true
		}
	}
	if !found {
		t.Error("no resume_failed progress event")
	}
	reqs := s.model.messagesRequests()
	if last := reqs[len(reqs)-1]; !strings.Contains(last.FirstUserText, "<recovery>") {
		t.Errorf("fresh session not seeded with recovery note: %q", last.FirstUserText)
	}
}

func TestFeasibilityAuthTokenFallback(t *testing.T) {
	s := newSpike(t, map[string]string{ConfigAuth: "auth_token"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, _, _ := s.start(t, harnesses.RecoveryDescriptor{}, nil)
	tr, err := a.Deliver(ctx, msg("m1", "Hello."))
	if err != nil || tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v %v", tr, err)
	}
	r := s.model.messagesRequests()[0]
	t.Logf("ANTHROPIC_AUTH_TOKEN: authorization=%q x-api-key=%q", r.Authorization, r.XAPIKey)
	if r.Authorization != "Bearer "+seatToken || r.XAPIKey != "" {
		t.Fatalf("auth headers %+v", r)
	}
}

func TestFeasibilityTokenRotation(t *testing.T) {
	s := newSpike(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, _, _ := s.start(t, harnesses.RecoveryDescriptor{}, nil)
	if tr, err := a.Deliver(ctx, msg("m1", "Hello.")); err != nil || tr.Status != harnesses.TurnCompleted {
		t.Fatalf("%+v %v", tr, err)
	}
	// Rotate the projected token; the next turn's process must use the new one.
	if err := os.WriteFile(s.dirs.TokenFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, _ := a.Deliver(ctx, msg("m2", "Hello."))
	reqs := s.model.messagesRequests()
	last := reqs[len(reqs)-1]
	t.Logf("after rotation: authorization=%q status=%s", last.Authorization, tr.Status)
	if last.Authorization != "Bearer rotated" || tr.Status != harnesses.TurnCompleted {
		t.Fatalf("token not re-read: %+v", last)
	}
}

func TestFeasibilityConformance(t *testing.T) {
	requireClaude(t)
	model := newStubModel(t)
	conformance.Run(t, conformance.Options{
		New:       func() harnesses.Adapter { return New() },
		QuickBody: "Just say hello.",
		SlowBody:  "SLOW please",
		Configure: func(t *testing.T, env *harnesses.Environment) {
			env.ModelProxyURL = model.URL + "/v1/model/model"
			env.Model = "claude-sonnet-4-5"
			env.HarnessConfig = map[string]string{ConfigInterruptGrace: "5s"}
			// The conformance tool server uses its own token.
			_ = os.WriteFile(env.TokenFile, []byte("conformance-token"), 0o600)
		},
		TurnTimeout:    2 * time.Minute,
		InterruptBound: 20 * time.Second,
	})
}

// TestFeasibilityAuthRejected records how Claude Code behaves when the model
// proxy rejects the seat token (e.g. a revoked seat): how long until the turn
// fails, and how many attempts it makes.
func TestFeasibilityAuthRejected(t *testing.T) {
	s := newSpike(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, _, _ := s.start(t, harnesses.RecoveryDescriptor{}, nil)
	if err := os.WriteFile(s.dirs.TokenFile, []byte("revoked"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	tr, err := a.Deliver(ctx, msg("m1", "Hello."))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("401 from model proxy: status=%s after %v, %d model requests, error=%q", tr.Status, time.Since(start).Round(time.Second), len(s.model.messagesRequests()), tr.Error)
	if tr.Status == harnesses.TurnCompleted {
		t.Fatal("turn completed despite 401")
	}
}
