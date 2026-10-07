package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
)

func parseFile(t *testing.T, name string) (*StreamParser, []ParsedEvent) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := &StreamParser{}
	var evs []ParsedEvent
	if err := readLines(bytes.NewReader(b), func(l []byte) { evs = append(evs, p.Parse(l)...) }); err != nil {
		t.Fatal(err)
	}
	return p, evs
}

func TestParseRecordedToolCallTurn(t *testing.T) {
	p, evs := parseFile(t, "turn_tool_call.jsonl")
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{harnesses.EventProgress, harnesses.EventToolRequest, harnesses.EventToolResult, harnesses.EventOutput}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	if p.Init == nil || p.Init.APIKeySource != "apiKeyHelper" || p.Init.Version != "2.1.289" || mcpStatus(p.Init) != "connected" {
		t.Fatalf("init %+v", p.Init)
	}
	req := evs[1]
	if req.CorrelationID != "toolu_stub_1" {
		t.Fatalf("tool_request %+v", req)
	}
	d := req.Data.(map[string]any)
	if d["name"] != "mcp__steadmesh__memory_write" {
		t.Fatalf("tool name %v", d["name"])
	}
	var input map[string]any
	_ = json.Unmarshal(d["input"].(json.RawMessage), &input)
	if input["store"] != "rep_alice" {
		t.Fatalf("input %v", input)
	}
	if evs[2].CorrelationID != "toolu_stub_1" || evs[2].Data.(map[string]any)["is_error"] != false {
		t.Fatalf("tool_result %+v", evs[2])
	}
	if p.Result == nil || p.Result.IsError || p.Result.Subtype != "success" || p.Result.Result != "memory written; replied via tool" {
		t.Fatalf("result %+v", p.Result)
	}
	if p.SessionID == "" || p.SessionID != p.Result.SessionID || p.ResumeFailed() {
		t.Fatalf("session %q result %+v", p.SessionID, p.Result)
	}
}

func TestParseRecordedResumeFailure(t *testing.T) {
	p, evs := parseFile(t, "resume_missing.jsonl")
	if !p.ResumeFailed() {
		t.Fatalf("resume failure not detected: %+v", p.Result)
	}
	if len(evs) != 1 || evs[0].Kind != harnesses.EventError {
		t.Fatalf("events %+v", evs)
	}
}

func TestParseRobustness(t *testing.T) {
	p := &StreamParser{}
	if evs := p.Parse([]byte("not json")); len(evs) != 1 || evs[0].Kind != harnesses.EventProgress {
		t.Fatalf("garbage: %+v", evs)
	}
	if evs := p.Parse([]byte(`{"type":"stream_event","event":{}}`)); len(evs) != 0 {
		t.Fatalf("unknown type: %+v", evs)
	}
	big := strings.Repeat("a", maxTextBytes*2)
	line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": big}}}})
	evs := p.Parse(line)
	if len(evs) != 1 || len(evs[0].Data.(map[string]any)["text"].(string)) != maxTextBytes {
		t.Fatal("text not truncated")
	}
	// A failed resumed run that produced output is not a resume failure.
	p2 := &StreamParser{AssistantMessages: 1, Result: &Result{IsError: true, Errors: []string{"No conversation found"}}}
	if p2.ResumeFailed() {
		t.Fatal("resume failure must require no model output")
	}
}

func TestReadLinesDropsOversizedLines(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"a":1}` + "\n")
	buf.WriteString(strings.Repeat("x", maxLineBytes+10) + "\n")
	buf.WriteString(`{"b":2}`)
	var got []string
	if err := readLines(&buf, func(l []byte) { got = append(got, strings.TrimSpace(string(l))) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("got %d lines: %.40v", len(got), got)
	}
}

func TestArgsAndConfig(t *testing.T) {
	a := New()
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = cfg
	a.stateDir = "/seat/runner/claude"
	a.env = harnesses.Environment{Model: &harnesses.ModelEndpoint{ID: "claude-sonnet-4-5", Settings: map[string]string{SettingEffort: "high"}}}
	args := strings.Join(a.Args("sess-1", "SYS"), " ")
	for _, want := range []string{"-p --output-format stream-json --verbose --bare", "--resume sess-1", "--mcp-config /seat/runner/claude/mcp.json --strict-mcp-config", "--settings /seat/runner/claude/settings.json", "--permission-mode bypassPermissions", "--permission-prompts none", "--append-system-prompt SYS", "--system-prompt-snapshot off", "--model claude-sonnet-4-5", "--effort high"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if strings.Contains(strings.Join(a.Args("", "SYS"), " "), "--resume") {
		t.Error("fresh run must not pass --resume")
	}
	if _, err := parseConfig(map[string]string{ConfigPermissionMode: "manual"}); err == nil {
		t.Error("a prompting permission mode must be rejected")
	}
}

func TestSettingsAndMCPConfig(t *testing.T) {
	env := harnesses.Environment{PlatformURL: "http://p:8080", TokenFile: "/var/run/steadmesh/token", RunnerDir: "/seat/runner", ToolCommand: "/usr/local/bin/steadmesh-tools", ExtraEnv: []string{"STEADMESH_SEAT_KEY=alice", "ANTHROPIC_API_KEY=leak"},
		Model: &harnesses.ModelEndpoint{ID: "claude-x", BaseURL: "http://127.0.0.1:7000/model/m", APIKey: "local"}}
	cfg, _ := parseConfig(nil)
	if _, ok := settings(cfg)["apiKeyHelper"]; ok {
		t.Fatal("settings must not carry a credential helper")
	}
	m := mcpConfig(env)["mcpServers"].(map[string]any)[MCPServerName].(map[string]any)
	e := m["env"].(map[string]string)
	if m["command"] != env.ToolCommand || e["STEADMESH_TOKEN_FILE"] != env.TokenFile || e["STEADMESH_SEAT_KEY"] != "alice" {
		t.Fatalf("mcp config %+v", m)
	}
	if _, leaked := e["ANTHROPIC_API_KEY"]; leaked {
		t.Fatal("non-STEADMESH env leaked into MCP config")
	}
	t.Setenv("ANTHROPIC_API_KEY", "should-not-leak")
	got := strings.Join(New().baseEnv(env), "\n")
	for _, want := range []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:7000/model/m", "ANTHROPIC_API_KEY=local", "ANTHROPIC_SMALL_FAST_MODEL=claude-x", "CLAUDE_CODE_SUBAGENT_MODEL=claude-x"} {
		if !strings.Contains(got, want) {
			t.Errorf("claude env lacks %s", want)
		}
	}
	if strings.Contains(got, "should-not-leak") {
		t.Fatal("the runner's ANTHROPIC_API_KEY leaked into claude env")
	}
}
