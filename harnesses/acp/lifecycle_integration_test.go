//go:build linux && integration

package acp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// The existing isolated-image test prefix includes this test. It exercises the
// actual lifecycle wrapper, not a fake launcher or protocol-only transport.
func TestExperimentalLauncherRestrictedLifecycle(t *testing.T) {
	l := ExperimentalLauncher{NodePath: os.Getenv("STEADMESH_ACP_NODE"), ACPPath: os.Getenv("STEADMESH_ACP_SCRIPT"), CodexPath: os.Getenv("STEADMESH_ACP_CODEX_BIN"), NeutralDir: os.Getenv("STEADMESH_ACP_NEUTRAL_DIR")}
	if l.NodePath == "" || l.ACPPath == "" || l.CodexPath == "" || l.NeutralDir == "" {
		t.Skip("set pinned runtime paths and STEADMESH_ACP_NEUTRAL_DIR")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	const token = "acp-restricted-lifecycle-fixture-token"
	dirs := conformance.NewDirs(t, token)
	l.PrivateParent = filepath.Join(dirs.Root, "private")
	if err := os.Mkdir(l.PrivateParent, 0700); err != nil {
		t.Fatal(err)
	}
	ts := conformance.NewToolServer(t, token)
	env := conformance.Env(dirs, ts, conformance.BuildTools(t))
	chain := conformance.NewModelChain(t, env.TokenFile, token, env.Generation)
	env.Model = chain.Endpoint("test-model", "gpt-stub-1", harnesses.APIOpenAIResponses, map[string]string{"reasoning_effort": "low"})
	target, err := url.Parse(env.Model.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	var mu sync.Mutex
	var bodies []string
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "fixture body read", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	defer recorder.Close()
	env.Model.BaseURL = recorder.URL
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "external network disabled", http.StatusForbidden)
	}))
	defer reject.Close()
	env.SandboxEnv = []string{"HTTP_PROXY=" + reject.URL, "HTTPS_PROXY=" + reject.URL, "NO_PROXY=127.0.0.1,localhost,::1"}
	captured := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), bodies...) }
	assertClean := func() {
		t.Helper()
		entries, err := os.ReadDir(l.PrivateParent)
		if err != nil || len(entries) != 0 {
			t.Fatalf("owned process directories not cleaned: %v %v", entries, err)
		}
	}
	// Each collector is joined only after Stop, proving closure ownership.
	startCollector := func(a *RestrictedAdapter) <-chan []harnesses.Event {
		out := make(chan []harnesses.Event, 1)
		go func() {
			var events []harnesses.Event
			for e := range a.Events() {
				events = append(events, e)
			}
			out <- events
		}()
		return out
	}
	assertEvents := func(events []harnesses.Event, expected map[string]string) {
		t.Helper()
		completions := map[string]int{}
		for _, e := range events {
			if expected[e.MessageID] == "" || expected[e.MessageID] != e.ExecutionID {
				t.Fatalf("event escaped delivery ownership: %+v", e)
			}
			if e.Kind == harnesses.EventCompletion {
				completions[e.MessageID]++
			}
		}
		for id := range expected {
			if completions[id] != 1 {
				t.Fatalf("message %s completion count %d", id, completions[id])
			}
		}
	}
	prepare := func(a *RestrictedAdapter, rec harnesses.RecoveryDescriptor) {
		t.Helper()
		before := len(captured())
		if err := a.Prepare(ctx, env); err != nil {
			t.Fatal(err)
		}
		r, err := a.StartOrResume(ctx, rec)
		if err != nil || r.Mode != harnesses.RecoveryPortableHandoff || r.SessionID != "" {
			t.Fatalf("portable start: %+v %v", r, err)
		}
		if len(captured()) != before {
			t.Fatal("Prepare or Start made model requests")
		}
		assertClean()
	}
	turn := func(a *RestrictedAdapter, id, body, contextMarker, handoffMarker string) {
		t.Helper()
		execution := "exec-" + id
		// The runner owns this file; bridge processes must keep the original runner
		// path, rather than accidentally reading their ephemeral CODEX_HOME tree.
		if err := os.WriteFile(filepath.Join(dirs.Runner, "execution"), []byte(execution), 0600); err != nil {
			t.Fatal(err)
		}
		chain.SetExecution(execution)
		before := len(captured())
		beforeCalls := len(chain.Calls())
		beforeTools := len(ts.Calls())
		d := harnesses.Delivery{ExecutionID: execution, Message: runtimeapi.Envelope{MessageID: id, Body: body, Origin: "human"}, Context: &runtimeapi.ParticipantContext{Revision: 2, Summary: contextMarker}}
		result, err := a.Deliver(ctx, d)
		if err != nil || result.Status != harnesses.TurnCompleted || result.SessionID != "" || result.Recovery == nil || result.Recovery.Mode != harnesses.RecoveryPortableHandoff {
			t.Fatalf("real delivery %s: %+v %v", id, result, err)
		}
		if strings.Contains(body, "readiness probe") {
			if !strings.Contains(result.Output, "done:") {
				t.Fatalf("probe output: %q", result.Output)
			}
		} else if !strings.Contains(result.Output, "hello from stub") {
			t.Fatalf("ordinary output: %q", result.Output)
		}
		raw := strings.Join(captured()[before:], "\n")
		for _, marker := range []string{contextMarker, handoffMarker, id, dirs.Workspace, "neutral directory"} {
			if !strings.Contains(raw, marker) {
				t.Fatalf("missing model context %q", marker)
			}
		}
		if len(chain.Calls()) == beforeCalls {
			t.Fatal("no forwarder call")
		}
		for _, call := range chain.Calls()[beforeCalls:] {
			if call.Token != token || call.Generation != "3" || call.Execution != execution {
				t.Fatalf("model authority: %+v", call)
			}
		}
		toolCalls := ts.Calls()[beforeTools:]
		if strings.Contains(body, "readiness probe") && (len(toolCalls) != 1 || toolCalls[0].Name != "self") {
			t.Fatalf("real readiness self: %+v", toolCalls)
		}
		for _, call := range toolCalls {
			if call.Generation != "3" || call.Execution != execution {
				t.Fatalf("tool authority: %+v", call)
			}
		}
		assertClean()
	}
	a := NewRestricted(l)
	defer a.Stop(context.Background())
	events := startCollector(a)
	prepare(a, harnesses.RecoveryDescriptor{Handoff: &runtimeapi.Handoff{Objective: "ACP_FIRST_HANDOFF_81602"}, UnknownOperations: []runtimeapi.Operation{{ID: "ACP_UNKNOWN_OPERATION_91201"}}})
	turn(a, "probe-first", harnesses.ProbePrompt, "ACP_FIRST_CONTEXT_61702", "ACP_FIRST_HANDOFF_81602")
	before := len(captured())
	turn(a, "ordinary-second", "hello on a fresh restricted turn", "ACP_SECOND_CONTEXT_91287", "ACP_FIRST_HANDOFF_81602")
	second := strings.Join(captured()[before:], "\n")
	for _, stale := range []string{"ACP_FIRST_CONTEXT_61702", "probe-first", harnesses.ProbePrompt} {
		if strings.Contains(second, stale) {
			t.Fatalf("fresh session inherited prior delivery %q", stale)
		}
	}
	if !strings.Contains(second, "ACP_UNKNOWN_OPERATION_91201") {
		t.Fatal("unknown operation missing from recovery bootstrap")
	}
	q, err := a.Quiesce(ctx)
	if err != nil || q.SafePoint != "turn_boundary" || q.SuspensionSupported {
		t.Fatalf("quiesce: %+v %v", q, err)
	}
	cp, err := a.Checkpoint(ctx)
	if err != nil || cp.CheckpointRef != "none" || cp.FormatVersion != portableFormat {
		t.Fatalf("checkpoint: %+v %v", cp, err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	assertEvents(<-events, map[string]string{"probe-first": "exec-probe-first", "ordinary-second": "exec-ordinary-second"})
	restored := NewRestricted(l)
	defer restored.Stop(context.Background())
	restoredEvents := startCollector(restored)
	prepare(restored, harnesses.RecoveryDescriptor{Session: &cp, Handoff: &runtimeapi.Handoff{Objective: "ACP_RESTORED_HANDOFF_62018"}})
	before = len(captured())
	turn(restored, "probe-restored", harnesses.ProbePrompt, "ACP_RESTORED_CONTEXT_42019", "ACP_RESTORED_HANDOFF_62018")
	restoredBody := strings.Join(captured()[before:], "\n")
	for _, stale := range []string{"ACP_FIRST_HANDOFF_81602", "ACP_SECOND_CONTEXT_91287", "ordinary-second"} {
		if strings.Contains(restoredBody, stale) {
			t.Fatalf("portable recovery inherited stale state %q", stale)
		}
	}
	if err := restored.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	assertEvents(<-restoredEvents, map[string]string{"probe-restored": "exec-probe-restored"})
	for _, r := range chain.Stub.Requests() {
		if r.Model != env.Model.ID || r.Key != conformance.UpstreamKey {
			t.Fatalf("model route: %+v", r)
		}
	}
	assertClean()
	t.Log("restricted lifecycle: nonlaunching preparation/start, real self and current seat fencing, fresh deliveries, portable checkpoint/recovery, context isolation and event ownership verified")
}
