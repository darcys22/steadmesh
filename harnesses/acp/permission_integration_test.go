//go:build linux && integration

package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/conformance"
)

// permissionAuditTransport only records fixture bytes. It cannot manufacture
// requests, alter responses, or override the production client's deny policy.
type permissionAuditTransport struct {
	io.ReadWriteCloser
	mu             sync.Mutex
	received, sent bytes.Buffer
	overflow       bool
}

func (a *permissionAuditTransport) record(dst *bytes.Buffer, b []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dst.Len()+len(b) > 4<<20 {
		a.overflow = true
		return
	}
	_, _ = dst.Write(b)
}
func (a *permissionAuditTransport) Read(b []byte) (int, error) {
	n, err := a.ReadWriteCloser.Read(b)
	a.record(&a.received, b[:n])
	return n, err
}
func (a *permissionAuditTransport) Write(b []byte) (int, error) {
	n, err := a.ReadWriteCloser.Write(b)
	a.record(&a.sent, b[:n])
	return n, err
}
func (a *permissionAuditTransport) denialError(marker string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.overflow {
		return fmt.Errorf("permission audit exceeded fixture capture bound")
	}
	type envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	requests := map[string]bool{}
	for _, line := range bytes.Split(a.received.Bytes(), []byte{'\n'}) {
		var e envelope
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Method == "session/request_permission" {
			if len(e.ID) == 0 || string(e.ID) == "null" {
				return fmt.Errorf("uncorrelatable permission request")
			}
			if !bytes.Contains(e.Params, []byte(marker)) {
				return fmt.Errorf("permission request not for fixture command: %s", e.Params)
			}
			requests[string(e.ID)] = false
		}
	}
	if len(requests) == 0 {
		return fmt.Errorf("native target never requested ACP permission; no denial proven")
	}
	for _, line := range bytes.Split(a.sent.Bytes(), []byte{'\n'}) {
		var e envelope
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if _, ok := requests[string(e.ID)]; !ok {
			continue
		}
		var result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(e.Result, &result) != nil || result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
			return fmt.Errorf("permission response did not deny: %s", line)
		}
		requests[string(e.ID)] = true
	}
	for id, denied := range requests {
		if !denied {
			return fmt.Errorf("missing correlated permission denial for %s", id)
		}
	}
	return nil
}

// TestExperimentalLauncherNativePermissionConformance tests the pinned native
// runtime through the production ACP Client. The test-only recording transport
// observes both the real permission request and its correlated cancellation.
// It does not register an adapter, enable approvals or change native sandboxing.
func TestExperimentalLauncherNativePermissionConformance(t *testing.T) {
	node, script, bin, neutral := os.Getenv("STEADMESH_ACP_NODE"), os.Getenv("STEADMESH_ACP_SCRIPT"), os.Getenv("STEADMESH_ACP_CODEX_BIN"), os.Getenv("STEADMESH_ACP_NEUTRAL_DIR")
	if node == "" || script == "" || bin == "" || neutral == "" {
		t.Skip("set pinned runtime paths and immutable neutral cwd")
	}
	if os.Geteuid() == 0 {
		t.Fatal("native permission fixture requires non-root image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	const token = "acp-permission-fixture-token"
	const execution = "acp-permission-conformance"
	dirs := conformance.NewDirs(t, token)
	for _, d := range []string{dirs.Home, dirs.Runner, dirs.Tmp} {
		if err := os.Chmod(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ts := conformance.NewToolServer(t, token)
	env := conformance.Env(dirs, ts, conformance.BuildTools(t))
	env.ExtraEnv = append(env.ExtraEnv, "STEADMESH_EXECUTION="+execution)
	chain := conformance.NewModelChain(t, env.TokenFile, token, env.Generation)
	chain.SetExecution(execution)
	env.Model = chain.Endpoint("test-model", "gpt-stub-1", harnesses.APIOpenAIResponses, map[string]string{"reasoning_effort": "low"})
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "external network disabled", 403) }))
	defer reject.Close()
	env.SandboxEnv = []string{"HTTP_PROXY=" + reject.URL, "HTTPS_PROXY=" + reject.URL, "NO_PROXY=127.0.0.1,localhost,::1"}
	for _, p := range []string{node, script, bin, env.ToolCommand} {
		if err := immutablePath(p, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := immutablePath(neutral, true); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCodex(ctx, env, bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyNeutralContext(&plan, neutral, dirs.Workspace); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.StartupConfigTOML, `approval_policy = "on-request"`) || !strings.Contains(plan.StartupConfigTOML, `sandbox_mode = "read-only"`) {
		t.Fatal("fixture changed required native permission policy")
	}
	home := filepath.Join(dirs.Runner, "acp-codex")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(plan.StartupConfigTOML), 0600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		bin  string
		args []string
		want string
	}{{node, []string{script, "--version"}, CodexACPName + " " + CodexACPVersion}, {bin, []string{"--version"}, "codex-cli " + CodexRuntimeVersion}} {
		if _, err := verifyRuntime(ctx, v.bin, v.args, plan.Env, neutral, v.want); err != nil {
			t.Fatal(err)
		}
	}
	process, err := launchOwned(node, []string{script}, plan.Env, neutral)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	audit := &permissionAuditTransport{ReadWriteCloser: process}
	client := NewClient(audit)
	defer client.Close()
	if err := client.Initialize(ctx, CodexACPName, CodexACPVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, plan.Session); err != nil {
		t.Fatal(err)
	}
	// A preliminary model request reveals the actual offered tool names. Do not
	// silently replace a missing native tool with a platform tool or text claim.
	if _, err := client.Prompt(ctx, "hello before native permission probe", nil); err != nil {
		t.Fatal(err)
	}
	var tools []string
	found := false
	for _, r := range chain.Stub.Requests() {
		tools = append(tools, r.Tools...)
		for _, name := range r.Tools {
			if name == "exec_command" || strings.HasSuffix(name, "/exec_command") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("pinned model does not expose expected exec_command schema; offered tools: %v", tools)
	}
	marker := filepath.Join(dirs.Workspace, "forbidden-native-write")
	// This native call asks for escalation specifically so the protocol must
	// reach the ACP permission callback. A sandbox failure alone cannot pass.
	shellPath := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
	args, _ := json.Marshal(map[string]any{"cmd": "printf native-permission-bypassed > " + shellPath, "sandbox_permissions": "require_escalated", "justification": "Fixture only: deny this attempted write.", "workdir": dirs.Workspace})
	stop, err := client.Prompt(ctx, "CALL exec_command "+string(args), nil)
	// Inspect the actual permission exchange even when the target terminates
	// the prompt. A textual/stop response alone never establishes denial.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := audit.denialError(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("forbidden write occurred or marker unreadable: %v", err)
	}
	if err != nil {
		t.Fatalf("native permission probe: %v", err)
	}
	if stop.StopReason != "cancelled" && stop.StopReason != "end_turn" {
		t.Fatalf("unexpected permission probe stop: %+v", stop)
	}
	sawResult := false
	for _, r := range chain.Stub.Requests() {
		if r.Model != env.Model.ID || r.Key != conformance.UpstreamKey {
			t.Fatalf("model routing violation: %+v", r)
		}
		if strings.HasPrefix(r.LastUser, "CALL exec_command ") && r.ToolResults > 0 {
			sawResult = true
		}
	}
	// ACP 2.5.0 maps our cancelled permission outcome to native decision
	// cancel, and maps the interrupted native turn to stopReason=cancelled.
	// There is no subsequent model request/tool result for that terminal path.
	// If the target instead continues normally, require the tool-result path.
	if stop.StopReason == "end_turn" && !sawResult {
		t.Fatal("continued turn lacks native tool result after permission denial")
	}
	for _, r := range chain.Calls() {
		if r.Token != token || r.Generation != "3" || r.Execution != execution {
			t.Fatalf("seat authority violation: %+v", r)
		}
	}
	if len(ts.Calls()) != 0 {
		t.Fatalf("native permission fixture unexpectedly used platform tools: %v", ts.Calls())
	}
	t.Logf("verified real native permission request, correlated ACP cancellation and absent marker for %s; prompt stop=%s", execution, stop.StopReason)
}

func TestPermissionAuditRequiresCorrelatedDenial(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"toolCall":{"rawInput":{"cmd":"touch /fixture-marker"}}}}` + "\n"
	cancelled := `{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}` + "\n"
	for _, tc := range []struct {
		name, in, out string
		wantOK        bool
	}{
		{"denied", request, cancelled, true},
		{"no request", "", cancelled, false},
		{"no response", request, "", false},
		{"wrong ID", request, strings.Replace(cancelled, `"id":7`, `"id":8`, 1), false},
		{"selected", request, strings.Replace(cancelled, `"cancelled"`, `"selected"`, 1), false},
		{"different command", strings.Replace(request, "/fixture-marker", "/other", 1), cancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &permissionAuditTransport{}
			a.received.WriteString(tc.in)
			a.sent.WriteString(tc.out)
			if err := a.denialError("/fixture-marker"); (err == nil) != tc.wantOK {
				t.Fatalf("got %v", err)
			}
		})
	}
}
