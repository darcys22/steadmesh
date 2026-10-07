package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance/modelstub"
	"github.com/darcys22/steadmesh/pkg/modelforward"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// UpstreamKey is the credential the stub platform proxy injects upstream.
const UpstreamKey = "upstream-key"

// ProxyCall is one request seen by the stub platform model proxy.
type ProxyCall struct {
	Path, Token, Generation, Execution string
	Status                             int
}

// ModelChain is a seat's model path in a test, as in a seat Pod: the harness
// calls the local forwarder (pkg/modelforward), which adds the seat token
// read from the token file; a stub platform proxy checks the token, swaps it
// for the upstream credential and passes the request to a scripted model
// (modelstub).
type ModelChain struct {
	Stub      *modelstub.Stub
	Platform  *httptest.Server
	Forwarder *modelforward.Forwarder

	mu        sync.Mutex
	tokens    map[string]bool
	calls     []ProxyCall
	execution atomic.Value // string
}

// NewModelChain starts a chain to the scripted model; its proxy accepts seatToken.
func NewModelChain(t testing.TB, tokenFile, seatToken string, generation int64) *ModelChain {
	t.Helper()
	stub := modelstub.New(UpstreamKey)
	upstream := httptest.NewServer(stub)
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del(runtimeapi.HeaderGeneration)
			pr.Out.Header.Del(runtimeapi.HeaderExecution)
			pr.Out.Header.Set("Authorization", "Bearer "+UpstreamKey)
		},
		FlushInterval: -1,
	}
	c := NewProxyChain(t, tokenFile, seatToken, generation, proxy)
	c.Stub = stub
	return c
}

// NewProxyChain starts a chain whose stub platform proxy checks the seat
// token and passes requests, with the /v1/model/<connection> prefix removed,
// to upstream: for example a model connector's Proxy, which injects a real
// credential.
func NewProxyChain(t testing.TB, tokenFile, seatToken string, generation int64, upstream http.Handler) *ModelChain {
	t.Helper()
	c := &ModelChain{tokens: map[string]bool{seatToken: true}}
	c.Platform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := ProxyCall{Path: r.URL.Path, Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			Generation: r.Header.Get(runtimeapi.HeaderGeneration), Execution: r.Header.Get(runtimeapi.HeaderExecution)}
		c.mu.Lock()
		ok := c.tokens[call.Token]
		c.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, runtimeapi.PathModelProxy) {
			call.Status = http.StatusNotFound
		} else if !ok {
			call.Status = http.StatusUnauthorized
		}
		c.mu.Lock()
		c.calls = append(c.calls, call)
		c.mu.Unlock()
		if call.Status != 0 {
			w.WriteHeader(call.Status)
			_ = json.NewEncoder(w).Encode(runtimeapi.Error{Code: "unauthenticated", Message: "bad seat token"})
			return
		}
		_, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, runtimeapi.PathModelProxy), "/")
		out := r.Clone(r.Context())
		out.URL.Path, out.URL.RawPath, out.RequestURI = "/"+rest, "", ""
		out.Header.Del("Authorization")
		upstream.ServeHTTP(w, out)
	}))
	t.Cleanup(c.Platform.Close)
	fwd, err := modelforward.Start(modelforward.Options{PlatformURL: c.Platform.URL, TokenFile: tokenFile,
		Generation: func() int64 { return generation }, Execution: c.currentExecution})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fwd.Close)
	c.Forwarder = fwd
	return c
}

func (c *ModelChain) currentExecution() string {
	id, _ := c.execution.Load().(string)
	return id
}

// SetExecution sets the execution header for following requests, as the
// seat runner does per delivery.
func (c *ModelChain) SetExecution(id string) { c.execution.Store(id) }

// AcceptToken makes the proxy accept another seat token.
func (c *ModelChain) AcceptToken(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[tok] = true
}

// Calls returns the requests the proxy saw.
func (c *ModelChain) Calls() []ProxyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ProxyCall(nil), c.calls...)
}

// Endpoint is the harness model environment for this chain.
func (c *ModelChain) Endpoint(conn, id, api string, settings map[string]string) *harnesses.ModelEndpoint {
	return &harnesses.ModelEndpoint{Connection: conn, ID: id, API: api, Settings: settings,
		BaseURL: c.Forwarder.BaseURL(conn), APIKey: modelforward.LocalAPIKey}
}

// ModelOptions configures RunModel.
type ModelOptions struct {
	// New returns a fresh, unprepared adapter.
	New func() harnesses.Adapter
	// API is the model API the harness is configured to speak.
	API string
	// Model is the model ID; Settings are model settings.
	Model    string
	Settings map[string]string
	// Configure may adjust the environment further (binary paths, config).
	Configure func(t *testing.T, env *harnesses.Environment)
	// TurnTimeout bounds a turn (default 2m).
	TurnTimeout time.Duration
	// CheckToolCall, when set, inspects the model requests of the turn in
	// which the model called a platform tool.
	CheckToolCall func(t *testing.T, reqs []modelstub.Request)
}

// RunModel runs the conformance suite for a model-backed harness against a
// scripted model over opts.API, through the real model forwarder: the full
// lifecycle suite, plus a platform tool call made by the model, the
// readiness probe turn, the model and destination actually used, and seat
// token rotation during an active session.
func RunModel(t *testing.T, opts ModelOptions) {
	if opts.TurnTimeout == 0 {
		opts.TurnTimeout = 2 * time.Minute
	}
	const conn = "llm"
	configure := func(chain **ModelChain) func(t *testing.T, env *harnesses.Environment) {
		return func(t *testing.T, env *harnesses.Environment) {
			tok, _ := os.ReadFile(env.TokenFile)
			c := NewModelChain(t, env.TokenFile, strings.TrimSpace(string(tok)), env.Generation)
			env.Model = c.Endpoint(conn, opts.Model, opts.API, opts.Settings)
			if opts.Configure != nil {
				opts.Configure(t, env)
			}
			if chain != nil {
				*chain = c
			}
		}
	}

	t.Run("Lifecycle", func(t *testing.T) {
		Run(t, Options{New: opts.New, QuickBody: "hello", SlowBody: "SLOW: keep working until interrupted",
			Configure: configure(nil), TurnTimeout: opts.TurnTimeout, InterruptBound: 30 * time.Second})
	})

	type rig struct {
		chain *ModelChain
		ts    *ToolServer
		dirs  Dirs
		a     harnesses.Adapter
		c     *collector
	}
	const token = "conformance-token"
	start := func(t *testing.T) *rig {
		r := &rig{ts: NewToolServer(t, token), dirs: NewDirs(t, token)}
		env := Env(r.dirs, r.ts, BuildTools(t))
		configure(&r.chain)(t, &env)
		r.a = opts.New()
		r.c = collect(r.a)
		ctx, cancel := context.WithTimeout(context.Background(), opts.TurnTimeout)
		defer cancel()
		if err := r.a.Prepare(ctx, env); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if _, err := r.a.StartOrResume(ctx, harnesses.RecoveryDescriptor{}); err != nil {
			t.Fatalf("StartOrResume: %v", err)
		}
		t.Cleanup(func() { _ = r.a.Stop(context.Background()) })
		return r
	}
	deliver := func(t *testing.T, r *rig, id, body string) harnesses.TurnResult {
		t.Helper()
		d := delivery(id, body)
		r.chain.SetExecution(d.ExecutionID)
		ctx, cancel := context.WithTimeout(context.Background(), opts.TurnTimeout)
		defer cancel()
		tr, err := r.a.Deliver(ctx, d)
		if err != nil {
			t.Fatalf("Deliver %s: %v", id, err)
		}
		if tr.Status != harnesses.TurnCompleted {
			t.Fatalf("turn %s: %+v", id, tr)
		}
		r.c.waitCompletion(t, d.ExecutionID)
		return tr
	}

	t.Run("ToolCallThroughModel", func(t *testing.T) {
		r := start(t)
		deliver(t, r, "tool1", `CALL memory.write {"store":"conformance","path":"notes/c.md","text":"from the model"}`)
		var found bool
		for _, c := range r.ts.Calls() {
			if c.Name == "memory.write" {
				var a map[string]any
				_ = json.Unmarshal(c.Arguments, &a)
				found = a["path"] == "notes/c.md" && a["text"] == "from the model"
				if c.Generation != "3" {
					t.Errorf("tool call generation %q", c.Generation)
				}
			}
		}
		if !found {
			t.Fatalf("memory.write not called through the tool bridge: %+v", r.ts.Calls())
		}
		// The harness saw the tool result and finished with the model's answer.
		var named bool
		for _, e := range r.c.forExecution("exec-tool1") {
			var d struct{ Name string }
			_ = json.Unmarshal(e.Data, &d)
			named = named || (e.Kind == harnesses.EventToolRequest && strings.HasSuffix(d.Name, "memory_write"))
		}
		if !named {
			t.Errorf("no tool_request event names memory_write")
		}
		reqs := r.chain.Stub.Requests()
		if len(reqs) < 2 || reqs[len(reqs)-1].ToolResults != 1 {
			t.Fatalf("model never received the tool result: %+v", reqs)
		}
		if opts.CheckToolCall != nil {
			opts.CheckToolCall(t, reqs)
		}
	})

	t.Run("ModelAndDestination", func(t *testing.T) {
		r := start(t)
		deliver(t, r, "dest1", "hello")
		var api int
		for _, q := range r.chain.Stub.Requests() {
			if q.API == "models" {
				continue
			}
			api++
			if q.API != opts.API || q.Model != opts.Model || q.Key != UpstreamKey {
				t.Errorf("upstream request %s: api %s model %q key %q; want %s %q %q", q.Path, q.API, q.Model, q.Key, opts.API, opts.Model, UpstreamKey)
			}
		}
		if api == 0 {
			t.Fatal("no model request reached the endpoint")
		}
		for _, c := range r.chain.Calls() {
			if c.Token != token || c.Generation != "3" || c.Status != 0 {
				t.Errorf("proxy call %+v: want the seat token and generation 3", c)
			}
		}
		if calls := r.chain.Calls(); len(calls) == 0 || calls[len(calls)-1].Execution != "exec-dest1" {
			t.Errorf("execution header not forwarded: %+v", calls)
		}
	})

	t.Run("ReadinessProbeTurn", func(t *testing.T) {
		r := start(t)
		deliver(t, r, "probe1", harnesses.ProbePrompt)
		var self bool
		for _, c := range r.ts.Calls() {
			self = self || c.Name == "self"
		}
		if !self {
			t.Fatalf("the probe turn did not call self: %+v", r.ts.Calls())
		}
	})

	t.Run("TokenRotationDuringSession", func(t *testing.T) {
		r := start(t)
		deliver(t, r, "rot1", "hello")
		// The projected token is replaced while the harness keeps running;
		// the old token stops working.
		r.chain.AcceptToken("rotated-token")
		r.chain.mu.Lock()
		delete(r.chain.tokens, token)
		r.chain.mu.Unlock()
		r.ts.SetToken("rotated-token")
		if err := os.WriteFile(r.dirs.TokenFile, []byte("rotated-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(r.chain.Calls())
		deliver(t, r, "rot2", `CALL memory.write {"store":"conformance","path":"notes/r.md","text":"after rotation"}`)
		after := r.chain.Calls()[before:]
		if len(after) == 0 {
			t.Fatal("no model requests after rotation")
		}
		for _, c := range after {
			if c.Token != "rotated-token" || c.Status != 0 {
				t.Fatalf("request after rotation used %q (status %d)", c.Token, c.Status)
			}
		}
	})
}

// HarnessBin returns the path of a pinned harness CLI for conformance runs:
// $STEADMESH_<NAME>_BIN, or bin/harnesses/<name> in the repository (fetched by
// build/harness-bins.sh). The test is skipped when neither exists.
func HarnessBin(t testing.TB, name string) string {
	t.Helper()
	if p := os.Getenv("STEADMESH_" + strings.ToUpper(name) + "_BIN"); p != "" {
		return p
	}
	_, file, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(file), "..", "..", "bin", "harnesses", name)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("%s CLI not available: run build/harness-bins.sh or set STEADMESH_%s_BIN", name, strings.ToUpper(name))
	}
	return p
}
