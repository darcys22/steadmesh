//go:build integration

package acp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
)

type testProcessTransport struct {
	in     io.WriteCloser
	out    io.ReadCloser
	cmd    *exec.Cmd
	done   <-chan struct{}
	once   sync.Once
	wireMu sync.Mutex
	wire   []byte
}

func (p *testProcessTransport) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	p.wireMu.Lock()
	if len(p.wire) < 32768 {
		p.wire = append(p.wire, b[:n]...)
	}
	p.wireMu.Unlock()
	return n, err
}
func (p *testProcessTransport) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *testProcessTransport) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		_ = p.out.Close()
		harnesses.KillProcessGroup(p.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	})
	return nil
}

// TestCodexACPProtocolConformance uses installed pinned runtimes only when
// explicitly selected. It sends fixture data to the local scripted model via
// the real Steadmesh forwarder, never to a paid/provider model endpoint.
func TestCodexACPProtocolConformance(t *testing.T) {
	node, script, bin := os.Getenv("STEADMESH_ACP_NODE"), os.Getenv("STEADMESH_ACP_SCRIPT"), os.Getenv("STEADMESH_ACP_CODEX_BIN")
	if node == "" || script == "" || bin == "" {
		t.Skip("set explicit pinned ACP node/script/Codex paths")
	}
	for _, p := range []string{node, script, bin} {
		if !filepath.IsAbs(p) {
			t.Fatal("absolute runtime paths required")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dirs := conformance.NewDirs(t, "acp-fixture-token")
	for _, dir := range []string{dirs.Home, dirs.Runner, dirs.Tmp} {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ts := conformance.NewToolServer(t, "acp-fixture-token")
	env := conformance.Env(dirs, ts, conformance.BuildTools(t))
	env.ExtraEnv = append(env.ExtraEnv, "STEADMESH_EXECUTION=acp-conformance")
	chain := conformance.NewModelChain(t, env.TokenFile, "acp-fixture-token", env.Generation)
	env.Model = chain.Endpoint("test-model", "gpt-stub-1", harnesses.APIOpenAIResponses, map[string]string{"reasoning_effort": "low"})
	chain.SetExecution("acp-conformance")
	// An external HTTP(S) attempt cannot reach a provider through proxy-aware
	// clients. Requests to local fixture services bypass this rejecting proxy.
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "external network disabled in ACP conformance", http.StatusForbidden)
	}))
	defer reject.Close()
	env.SandboxEnv = []string{"HTTP_PROXY=" + reject.URL, "HTTPS_PROXY=" + reject.URL, "NO_PROXY=127.0.0.1,localhost,::1"}
	plan, err := PlanCodex(ctx, env, bin)
	if err != nil {
		t.Fatal(err)
	}
	// Configuration is inspected before starting an agent session.
	joined := strings.Join(plan.Env, "\n")
	if !strings.Contains(joined, env.Model.BaseURL+"/v1") || strings.Contains(joined, "OPENAI_API_KEY=") {
		t.Fatal("unsafe generated provider configuration")
	}
	if err := os.MkdirAll(filepath.Join(env.RunnerDir, "acp-codex"), 0700); err != nil {
		t.Fatal(err)
	}
	// This is a fresh fixture-owned directory, not user configuration. The
	// pinned ACP target needs the custom provider at app-server startup too.
	f, err := os.OpenFile(filepath.Join(env.RunnerDir, "acp-codex", "config.toml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(plan.StartupConfigTOML); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		bin  string
		args []string
		want string
	}{
		{node, []string{script, "--version"}, CodexACPName + " " + CodexACPVersion},
		{bin, []string{"--version"}, "codex-cli " + CodexRuntimeVersion},
	} {
		cmd := exec.CommandContext(ctx, check.bin, check.args...)
		cmd.Env = plan.Env
		cmd.Dir = env.RunnerDir
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != check.want {
			t.Fatalf("unverified runtime version: %q %v", out, err)
		}
	}

	stderr, err := os.CreateTemp(dirs.Root, "acp-stderr-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	cmd := exec.Command(node, script)
	cmd.Env = plan.Env
	cmd.Dir = env.RunnerDir
	cmd.Stderr = stderr
	harnesses.NewProcessGroup(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = outWriter
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		_ = outWriter.Close()
		t.Fatal(err)
	}
	_ = outWriter.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	transport := &testProcessTransport{in: in, out: out, cmd: cmd, done: done}
	defer transport.Close()
	fail := func(t *testing.T, stage string, err error) {
		_ = transport.Close()
		data, _ := os.ReadFile(stderr.Name())
		transport.wireMu.Lock()
		wire := string(transport.wire)
		transport.wireMu.Unlock()
		t.Fatalf("%s: %v\nstderr: %s\nfixture wire: %s", stage, err, data, wire)
	}
	client := NewClient(transport)
	defer client.Close()
	initialized := false
	t.Run("Initialize", func(t *testing.T) {
		if err := client.Initialize(ctx, CodexACPName, CodexACPVersion); err != nil {
			fail(t, "initialize", err)
		}
		initialized = true
		if len(chain.Stub.Requests()) != 0 {
			t.Fatal("initialization made a model request")
		}
		t.Log("verified pinned target identity and ACP v1 without model requests")
	})
	if !initialized {
		return
	}
	t.Run("PromptViaForwarder", func(t *testing.T) {
		if _, err := client.NewSession(ctx, plan.Session); err != nil {
			fail(t, "session/new", err)
		}
		if err := client.pinCodexSessionTitle(ctx); err != nil {
			fail(t, "pin session title", err)
		}
		var output strings.Builder
		stop, err := client.Prompt(ctx, "hello", func(o Observation) { output.WriteString(o.Text) })
		if err != nil {
			fail(t, "prompt", err)
		}
		if stop.StopReason != "end_turn" || !strings.Contains(output.String(), "hello from stub") {
			t.Fatalf("unexpected output: %+v %q", stop, output.String())
		}
		requests := chain.Stub.Requests()
		if len(requests) == 0 {
			t.Fatal("model forwarder was not used")
		}
		for _, r := range requests {
			if r.Model != env.Model.ID || r.Key != conformance.UpstreamKey {
				t.Fatalf("model routing/credential violation: %+v", r)
			}
		}
		for _, r := range chain.Calls() {
			if r.Token != "acp-fixture-token" || r.Generation != "3" || r.Execution != "acp-conformance" {
				t.Fatalf("seat authority lost: %+v", r)
			}
		}
		t.Logf("pinned ACP round trip passed through local forwarder; %d model requests", len(requests))
	})
	t.Run("ReadOnlyToolBridge", func(t *testing.T) {
		var output strings.Builder
		stop, err := client.Prompt(ctx, harnesses.ProbePrompt, func(o Observation) { output.WriteString(o.Text) })
		if err != nil {
			fail(t, "tool probe", err)
		}
		if stop.StopReason != "end_turn" {
			t.Fatalf("tool probe stop: %+v", stop)
		}
		calls := ts.Calls()
		if len(calls) != 1 || calls[0].Name != "self" || calls[0].Generation != "3" || calls[0].Execution != "acp-conformance" {
			t.Fatalf("platform self tool was not called: calls=%+v output=%q requests=%+v", ts.Calls(), output.String(), chain.Stub.Requests())
		}
		t.Log("read-only platform self tool reached fixture bridge")
	})
	// Keep the session alive after the turn so title/suggestion background calls
	// cannot hide behind immediate process teardown.
	time.Sleep(time.Second)
	for _, request := range chain.Stub.Requests() {
		for _, tool := range request.Tools {
			for _, native := range []string{"spawn_agent", "send_input", "wait_agent", "resume_agent", "close_agent", "spawn_agents_on_csv"} {
				if tool == native || tool == "functions/"+native {
					t.Fatalf("unexpected native subagent tool: %s", tool)
				}
			}
		}
		if request.Model != env.Model.ID {
			t.Fatalf("auxiliary request escaped model pin: %+v", request)
		}
	}
}
