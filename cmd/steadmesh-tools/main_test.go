package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

type fakePlatform struct {
	calls []string
	gens  []string
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer seat-token" {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"code":"unauthenticated"}`))
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == runtimeapi.PathTools:
		_ = json.NewEncoder(w).Encode(runtimeapi.ToolList{Tools: []runtimeapi.ToolDescriptor{
			{Name: "memory.write", Description: "write", InputSchema: json.RawMessage(`{"type":"object","properties":{"store":{"type":"string"}}}`)},
			{Name: "self", Description: "self", InputSchema: nil},
			{Name: "messages.reply", Description: "reply", InputSchema: json.RawMessage(`{"type":"object"}`)},
		}})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, runtimeapi.PathToolCall):
		name := strings.TrimPrefix(r.URL.Path, runtimeapi.PathToolCall)
		var req runtimeapi.ToolCallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls = append(f.calls, name+" "+string(req.Arguments))
		f.gens = append(f.gens, r.Header.Get(runtimeapi.HeaderGeneration)+"/"+r.Header.Get(runtimeapi.HeaderExecution))
		switch name {
		case "memory.write":
			_, _ = w.Write([]byte(`{"content":{"id":"rec-1","revision":1}}`))
		case "messages.reply":
			_, _ = w.Write([]byte(`{"content":{"error":"route denied"},"is_error":true}`))
		case "self":
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"code":"fenced","message":"generation 1 < 2"}`))
		}
	default:
		w.WriteHeader(404)
	}
}

func setup(t *testing.T) *fakePlatform {
	t.Helper()
	fp := &fakePlatform{}
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	_ = os.WriteFile(tf, []byte("seat-token"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "generation"), []byte("3"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "execution"), []byte("exec-9"), 0o600)
	t.Setenv(client.EnvPlatformURL, srv.URL)
	t.Setenv(client.EnvTokenFile, tf)
	t.Setenv(client.EnvRunnerDir, dir)
	t.Setenv(client.EnvGeneration, "")
	t.Setenv(client.EnvExecution, "")
	return fp
}

func TestMCPNameMapping(t *testing.T) {
	cases := map[string]string{"memory.write": "memory_write", "self": "self", "connections.invoke": "connections_invoke", "a.b.c": "a_b_c"}
	for in, want := range cases {
		if got := MCPName(in); got != want {
			t.Errorf("MCPName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLICall(t *testing.T) {
	fp := setup(t)
	var out, errb bytes.Buffer
	code := run([]string{"call", "memory.write", `{"store":"rep_alice","title":"t","body":"b"}`}, nil, &out, &errb)
	if code != exitOK || strings.TrimSpace(out.String()) != `{"id":"rec-1","revision":1}` {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	if fp.gens[0] != "3/exec-9" {
		t.Fatalf("headers %v", fp.gens)
	}
	out.Reset()
	code = run([]string{"call", "messages.reply", "-"}, strings.NewReader(`{"message_id":"m"}`), &out, &errb)
	if code != exitToolError || !strings.Contains(out.String(), "route denied") {
		t.Fatalf("is_error: code %d out %q", code, out.String())
	}
	code = run([]string{"call", "self"}, nil, &out, &errb)
	if code != exitFenced {
		t.Fatalf("fenced: code %d", code)
	}
	if code := run([]string{"call", "self", "{bad"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("bad json: %d", code)
	}
	out.Reset()
	if code := run([]string{"list"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "memory.write") {
		t.Fatalf("list: %d %s", code, out.String())
	}
}

func TestMCPServerProxies(t *testing.T) {
	fp := setup(t)
	c, err := client.FromEnv("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = serveMCP(ctx, c, st, io.Discard) }()
	cl := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range lt.Tools {
		got[tl.Name] = true
		if m, ok := tl.InputSchema.(map[string]any); !ok || m["type"] != "object" {
			t.Errorf("%s schema %v", tl.Name, tl.InputSchema)
		}
	}
	for _, n := range []string{"memory_write", "self", "messages_reply"} {
		if !got[n] {
			t.Errorf("missing tool %s in %v", n, got)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "memory_write", Arguments: map[string]any{"store": "s"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; txt != `{"id":"rec-1","revision":1}` {
		t.Fatalf("content %q", txt)
	}
	if fp.calls[0] != `memory.write {"store":"s"}` {
		t.Fatalf("proxied %v", fp.calls)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "messages_reply", Arguments: map[string]any{}})
	if err != nil || !res.IsError {
		t.Fatalf("is_error not mapped: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "self", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "fenced") {
		t.Fatalf("fenced not mapped: %v %+v", err, res)
	}
}
