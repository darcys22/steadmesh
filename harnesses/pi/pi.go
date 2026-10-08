// Package pi is the adapter for the Pi coding agent (pi.dev,
// @earendil-works/pi-coding-agent). It drives one long-running
// `pi --mode rpc` process over JSONL on stdio (CONTRACT.md):
//
//   - PI_CODING_AGENT_DIR and the session directory live under <runner>/pi
//     on the seat volume, so sessions survive Pod replacement;
//   - models.json declares a "steadmesh" provider at the seat's model
//     forwarder, speaking the profile's API (anthropic-messages,
//     openai-responses or openai-completions) with a placeholder key;
//   - mcp.json adds steadmesh-tools with exposure "direct" (the default,
//     codemode, would hide the tools);
//   - a turn is prompt ... agent_settled; interruption is abort.
//
// Pi asks for no tool approvals; the seat Pod is the boundary. Checkpoint =
// session file + Pi version (application_checkpoint).
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

const (
	// AdapterName is the harness profile adapter value.
	AdapterName = "pi"
	// PinnedVersion is the verified Pi release (CONTRACT.md).
	PinnedVersion = "1.0.4"
	// SessionFormatPrefix prefixes Checkpoint.FormatVersion.
	SessionFormatPrefix = "pi-session/"
	// MCPServerName is the tool bridge's server name; tools appear as
	// mcp__steadmesh__<tool>.
	MCPServerName = "steadmesh"
	// ProviderID is the models.json provider for the seat's model.
	ProviderID = "steadmesh"

	defaultInterruptGrace = 15 * time.Second
	startTimeout          = 60 * time.Second
)

// Model settings (model.settings).
const (
	SettingThinking      = "thinking"       // --thinking level
	SettingContextWindow = "context_window" // models.json contextWindow
	SettingMaxTokens     = "max_tokens"     // models.json maxTokens
	SettingReasoning     = "reasoning"      // models.json reasoning (true for thinking models)
)

// Harness profile config keys.
const (
	ConfigBinary         = "pi_bin"          // default "pi"
	ConfigVersion        = "version"         // when set, Prepare requires exactly this version
	ConfigInterruptGrace = "interrupt_grace" // Go duration, default 15s
)

// piAPI maps platform API names to Pi's.
var piAPI = map[string]string{
	harnesses.APIAnthropicMessages: "anthropic-messages",
	harnesses.APIOpenAIResponses:   "openai-responses",
	harnesses.APIOpenAIChat:        "openai-completions",
}

func init() {
	harnesses.Register(harnesses.Descriptor{
		Name:       AdapterName,
		APIs:       []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat, harnesses.APIAnthropicMessages},
		NeedsModel: true,
		Settings: map[string]harnesses.Setting{
			SettingThinking:      {Description: "thinking level (--thinking)", Values: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}},
			SettingContextWindow: {Description: "the model's context window in tokens (default 128000)"},
			SettingMaxTokens:     {Description: "maximum output tokens per response (default 16384)"},
			SettingReasoning:     {Description: "whether the model supports thinking", Values: []string{"true", "false"}},
		},
		Capabilities: []string{"tools", "mcp", "event_stream", "session_resume", "interrupt", "application_checkpoint"},
		New:          func() harnesses.Adapter { return New() },
	})
}

// SessionState is <runner>/pi-state.json.
type SessionState struct {
	SessionFile    string    `json:"session_file"`
	SessionID      string    `json:"session_id"`
	PiVersion      string    `json:"pi_version"`
	ConfigRevision string    `json:"config_revision,omitempty"`
	Turns          int       `json:"turns"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Adapter runs Pi.
type Adapter struct {
	mu        sync.Mutex
	env       harnesses.Environment
	bin       string
	version   string
	dir       string // <runner>/pi
	grace     time.Duration
	bus       *harnesses.EventBus
	guard     harnesses.TurnGuard
	started   bool
	stopping  bool
	session   SessionState
	recovery  *harnesses.ResumeResult
	handoff   *runtimeapi.Handoff
	bootstrap string

	proc *process

	// Tap, when set before StartOrResume, receives every raw stdout record.
	Tap func(line []byte)
}

// New returns a Pi adapter.
func New() *Adapter { return &Adapter{bus: harnesses.NewEventBus(4096)} }

var _ harnesses.Adapter = (*Adapter)(nil)

func (a *Adapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{
		Adapter:              AdapterName,
		Version:              PinnedVersion,
		ToolTransports:       []string{"mcp"},
		EventStreaming:       true,
		Interruption:         "cooperative",
		RecoveryModes:        []string{harnesses.RecoveryFresh, harnesses.RecoveryNativeResume, harnesses.RecoveryPortableHandoff},
		CheckpointGuarantees: []string{harnesses.GuaranteeApplication},
		SessionFormat:        SessionFormatPrefix + PinnedVersion,
	}
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func (a *Adapter) Prepare(ctx context.Context, env harnesses.Environment) error {
	if env.Model == nil || env.Model.BaseURL == "" || env.Model.ID == "" {
		return errors.New("pi: a model is required")
	}
	if _, ok := piAPI[env.Model.API]; !ok {
		return fmt.Errorf("pi: unsupported API %q", env.Model.API)
	}
	if env.ToolCommand == "" || env.RunnerDir == "" || env.WorkspaceDir == "" || env.HomeDir == "" {
		return errors.New("pi: ToolCommand, workspace, home and runner dirs are required")
	}
	grace := defaultInterruptGrace
	if v := env.HarnessConfig[ConfigInterruptGrace]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("pi: %s: %w", ConfigInterruptGrace, err)
		}
		grace = d
	}
	dir := filepath.Join(env.RunnerDir, "pi")
	for _, d := range []string{env.WorkspaceDir, env.HomeDir, filepath.Join(dir, "agent"), filepath.Join(dir, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("pi: %w", err)
		}
	}
	bin := env.HarnessConfig[ConfigBinary]
	if bin == "" {
		bin = "pi"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("pi: pi binary: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, "--version")
	cmd.Env = baseEnv(env, dir)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("pi: pi --version: %w", err)
	}
	version := versionRe.FindString(string(out))
	if version == "" {
		return fmt.Errorf("pi: cannot parse version from %q", strings.TrimSpace(string(out)))
	}
	if want := env.HarnessConfig[ConfigVersion]; want != "" && want != version {
		return fmt.Errorf("pi: pinned version %s required, found %s", want, version)
	}
	models, err := ModelsJSON(env)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "agent", "models.json"), models); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "agent", "mcp.json"), MCPJSON(env)); err != nil {
		return err
	}
	// Never load project-level .pi resources from the workspace.
	if err := writeFile(filepath.Join(dir, "agent", "settings.json"), []byte(`{"defaultProjectTrust":"never","enableInstallTelemetry":false}`)); err != nil {
		return err
	}
	a.mu.Lock()
	a.env, a.bin, a.version, a.dir, a.grace = env, path, version, dir, grace
	a.mu.Unlock()
	return nil
}

// ModelsJSON renders models.json: one provider at the model forwarder.
func ModelsJSON(env harnesses.Environment) ([]byte, error) {
	m := env.Model
	base := strings.TrimSuffix(m.BaseURL, "/")
	if m.API != harnesses.APIAnthropicMessages {
		// The OpenAI APIs take the /v1 base; Pi's Anthropic client adds /v1 itself.
		base += "/v1"
	}
	model := map[string]any{"id": m.ID}
	for key, field := range map[string]string{SettingContextWindow: "contextWindow", SettingMaxTokens: "maxTokens"} {
		if v := m.Settings[key]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("pi: setting %s must be a positive integer", key)
			}
			model[field] = n
		}
	}
	if v := m.Settings[SettingReasoning]; v != "" {
		model["reasoning"] = v == "true"
	}
	return json.MarshalIndent(map[string]any{"providers": map[string]any{
		ProviderID: map[string]any{"baseUrl": base, "api": piAPI[m.API], "apiKey": m.APIKey, "models": []any{model}},
	}}, "", "  ")
}

// MCPJSON renders mcp.json with the tool bridge exposed directly.
func MCPJSON(env harnesses.Environment) []byte {
	e := map[string]string{
		"STEADMESH_PLATFORM_URL": env.PlatformURL,
		"STEADMESH_TOKEN_FILE":   env.TokenFile,
		"STEADMESH_RUNNER_DIR":   env.RunnerDir,
	}
	for _, kv := range env.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "STEADMESH_") {
			e[k] = v
		}
	}
	servers := map[string]any{
		MCPServerName: map[string]any{"type": "stdio", "command": env.ToolCommand, "args": []string{"mcp"}, "env": e, "exposure": "direct", "timeout": 600},
	}
	for _, s := range env.MCPServers {
		servers[s.Name] = map[string]any{"type": "stdio", "command": s.Command, "args": s.Args, "env": s.Env, "exposure": "direct", "timeout": 600}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return b
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("pi: write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// baseEnv is the allow-listed environment for pi; no provider credential
// from the runner's environment can leak in.
func baseEnv(env harnesses.Environment, dir string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	out := []string{"PATH=" + path, "HOME=" + env.HomeDir, "LANG=C.UTF-8",
		"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "agent"), "PI_OFFLINE=1", "PI_TELEMETRY=0"}
	if env.TmpDir != "" {
		out = append(out, "TMPDIR="+env.TmpDir)
	}
	for _, kv := range env.ExtraEnv {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "STEADMESH_") {
			out = append(out, kv)
		}
	}
	return append(out, env.SandboxEnv...)
}

// ---- process ------------------------------------------------------------------

// record is one stdout record: a command response or an event.
type record struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Success bool            `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	raw     []byte
}

type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	done   chan struct{}
	stderr *tailBuffer

	mu      sync.Mutex
	nextID  int
	pending map[string]chan record
	sink    chan record
}

func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *process) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// call sends a command and waits for its response.
func (p *process) call(ctx context.Context, cmd map[string]any) (record, error) {
	p.mu.Lock()
	p.nextID++
	id := "c" + strconv.Itoa(p.nextID)
	ch := make(chan record, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	cmd["id"] = id
	if err := p.send(cmd); err != nil {
		return record{}, err
	}
	select {
	case r := <-ch:
		if !r.Success {
			return r, fmt.Errorf("pi %s: %s", r.Command, r.Error)
		}
		return r, nil
	case <-p.done:
		return record{}, errors.New("pi exited")
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return record{}, ctx.Err()
	}
}

func (p *process) subscribe() (<-chan record, func()) {
	ch := make(chan record, 4096)
	p.mu.Lock()
	p.sink = ch
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		if p.sink == ch {
			p.sink = nil
		}
		p.mu.Unlock()
	}
}

func (p *process) read(r io.Reader, tap func([]byte)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if tap != nil {
			tap(line)
		}
		var rec record
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		rec.raw = line
		switch {
		case rec.Type == "response" && rec.ID != "":
			p.mu.Lock()
			ch := p.pending[rec.ID]
			delete(p.pending, rec.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- rec
			}
		case rec.Type == "extension_ui_request":
			// An unattended seat answers every dialog by cancelling it.
			_ = p.send(map[string]any{"type": "extension_ui_response", "id": rec.ID, "cancelled": true})
		default:
			p.mu.Lock()
			sink := p.sink
			p.mu.Unlock()
			if sink != nil {
				select {
				case sink <- rec:
				default:
				}
			}
		}
	}
}

func (p *process) kill() {
	if p.cmd.Process != nil {
		harnesses.KillProcessGroup(p.cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}

// startLocked spawns pi, resuming the session file when there is one.
func (a *Adapter) startLocked(ctx context.Context) error {
	args := []string{"--mode", "rpc", "--provider", ProviderID, "--model", a.env.Model.ID,
		"--session-dir", filepath.Join(a.dir, "sessions"), "--no-approve", "--append-system-prompt", a.bootstrap}
	if t := a.env.Model.Settings[SettingThinking]; t != "" {
		args = append(args, "--thinking", t)
	}
	if a.session.SessionFile != "" {
		args = append(args, "--session", a.session.SessionFile)
	}
	cmd := exec.Command(a.bin, args...)
	cmd.Dir = a.env.WorkspaceDir
	cmd.Env = baseEnv(a.env, a.dir)
	harnesses.NewProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	p := &process{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: &tailBuffer{}, pending: map[string]chan record{}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pi: start: %w", err)
	}
	readDone := make(chan struct{})
	go func() { p.read(stdout, a.Tap); close(readDone) }()
	go func() { <-readDone; _ = cmd.Wait(); close(p.done) }()
	a.proc = p
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	st, err := p.call(sctx, map[string]any{"type": "get_state"})
	if err != nil {
		p.kill()
		a.proc = nil
		return fmt.Errorf("pi: get_state: %w%s", err, stderrNote(p))
	}
	var state struct {
		SessionFile string `json:"sessionFile"`
		SessionID   string `json:"sessionId"`
	}
	_ = json.Unmarshal(st.Data, &state)
	a.session.SessionFile, a.session.SessionID = state.SessionFile, state.SessionID
	return nil
}

func stderrNote(p *process) string {
	if s := strings.TrimSpace(p.stderr.String()); s != "" {
		return ": " + harnesses.TruncateUTF8(lastLines(s, 5), 2048)
	}
	return ""
}

func (a *Adapter) killLocked() {
	if a.proc != nil {
		a.proc.kill()
		a.proc = nil
	}
}

// ---- state --------------------------------------------------------------------

func (a *Adapter) statePath() string { return filepath.Join(a.env.RunnerDir, "pi-state.json") }

func (a *Adapter) loadSession() (SessionState, bool) {
	b, err := os.ReadFile(a.statePath())
	if err != nil {
		return SessionState{}, false
	}
	var s SessionState
	if json.Unmarshal(b, &s) != nil || s.SessionFile == "" {
		return SessionState{}, false
	}
	return s, true
}

func (a *Adapter) saveSessionLocked() error {
	a.session.UpdatedAt = time.Now().UTC()
	b, _ := json.MarshalIndent(a.session, "", "  ")
	return writeFile(a.statePath(), b)
}

// sessionFileFor finds a session file in the session directory by id or path.
func (a *Adapter) sessionFileFor(ref string) string {
	if ref == "" || ref == "none" {
		return ""
	}
	if filepath.IsAbs(ref) {
		if _, err := os.Stat(ref); err == nil {
			return ref
		}
		return ""
	}
	if strings.ContainsAny(ref, `/\*?[`) {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(a.dir, "sessions", "*_"+ref+".jsonl"))
	if len(m) > 0 {
		return m[0]
	}
	return ""
}

func (a *Adapter) StartOrResume(ctx context.Context, rec harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bin == "" {
		return harnesses.ResumeResult{}, errors.New("pi: Prepare has not been called")
	}
	a.started = true
	a.handoff = rec.Handoff
	a.bootstrap = harnesses.RenderBootstrap(a.env, "")
	fresh := SessionState{PiVersion: a.version, ConfigRevision: a.env.ConfigRevision}
	res := harnesses.ResumeResult{Mode: harnesses.RecoveryFresh}
	fallback := func(note string) {
		a.session = fresh
		_ = os.Remove(a.statePath())
		res = harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
		a.recovery = &res
	}
	switch s, ok := a.loadSession(); {
	case ok && a.sessionFileFor(s.SessionFile) != "":
		a.session = s
		res = harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: s.SessionID}
		if s.PiVersion != "" && s.PiVersion != a.version {
			res.Note = fmt.Sprintf("session written by Pi %s; resuming with %s", s.PiVersion, a.version)
		}
	case ok:
		fallback(fmt.Sprintf("Pi session %s is missing from the seat volume; started a new session from the portable handoff (not a continuation of the previous native session)", s.SessionID))
	case rec.Session != nil && rec.Session.HarnessAdapter != AdapterName:
		fallback(fmt.Sprintf("native session from adapter %q (format %s) cannot be converted to Pi; started a new session from the portable handoff", rec.Session.HarnessAdapter, rec.Session.FormatVersion))
	case rec.Session != nil && rec.Session.CheckpointRef != "none" && a.sessionFileFor(rec.Session.CheckpointRef) != "":
		a.session = fresh
		a.session.SessionFile = a.sessionFileFor(rec.Session.CheckpointRef)
		res = harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: rec.Session.CheckpointRef}
	case rec.Session != nil && rec.Session.CheckpointRef != "none":
		fallback(fmt.Sprintf("Pi session %s recorded by the platform is not on this volume; started a new session from the portable handoff (not a continuation of the previous native session)", rec.Session.CheckpointRef))
	default:
		a.session = fresh
	}
	if err := a.startLocked(ctx); err != nil {
		return harnesses.ResumeResult{}, err
	}
	if res.Mode == harnesses.RecoveryNativeResume {
		res.SessionID = a.session.SessionID
	}
	return res, a.saveSessionLocked()
}

func (a *Adapter) Events() <-chan harnesses.Event { return a.bus.C() }

func (a *Adapter) Quiesce(ctx context.Context) (harnesses.QuiesceResult, error) {
	return a.guard.Quiesce(ctx), nil
}

func (a *Adapter) Checkpoint(ctx context.Context) (runtimeapi.Checkpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return runtimeapi.Checkpoint{}, harnesses.ErrNotStarted
	}
	ref := a.session.SessionID
	if ref == "" || a.session.Turns == 0 {
		// Pi writes a session file only once it has content.
		ref = "none"
	}
	return runtimeapi.Checkpoint{HarnessAdapter: AdapterName, FormatVersion: SessionFormatPrefix + a.version, CheckpointRef: ref, Guarantee: harnesses.GuaranteeApplication}, nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	if ctx.Err() != nil {
		a.killLocked()
	}
	a.mu.Unlock()
	done := a.guard.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(a.grace + 5*time.Second):
		}
	}
	a.mu.Lock()
	if a.proc != nil {
		// Closing stdin is Pi's orderly shutdown.
		_ = a.proc.stdin.Close()
		select {
		case <-a.proc.done:
		case <-time.After(5 * time.Second):
		}
	}
	a.killLocked()
	a.mu.Unlock()
	a.bus.Close()
	return nil
}

// ---- turns --------------------------------------------------------------------

func (a *Adapter) prompt(d harnesses.Delivery, recovery *harnesses.ResumeResult) string {
	var sb strings.Builder
	if recovery != nil {
		sb.WriteString("<recovery>\n" + recovery.Note + "\n")
		if a.handoff != nil {
			sb.WriteString(harnesses.RenderHandoff(*a.handoff))
		}
		sb.WriteString("Use memory.search, messages.history and operations.get to rebuild context before acting.\n</recovery>\n\n")
	}
	sb.WriteString(harnesses.RenderEnvelope(d))
	return sb.String()
}

// event is the subset of Pi events the adapter reads (CONTRACT.md §2).
type event struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
	Message    *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

func (a *Adapter) Deliver(ctx context.Context, d harnesses.Delivery) (harnesses.TurnResult, error) {
	a.mu.Lock()
	started := a.started
	a.mu.Unlock()
	if !started {
		return harnesses.TurnResult{}, harnesses.ErrNotStarted
	}
	tctx, end, err := a.guard.Begin(ctx)
	if err != nil {
		return harnesses.TurnResult{}, err
	}
	defer end()
	start := time.Now()
	emit := func(kind, corr string, data any) {
		a.bus.Emit(tctx, harnesses.Event{Kind: kind, ExecutionID: d.ExecutionID, MessageID: d.Message.MessageID, CorrelationID: corr, Data: harnesses.MustJSON(data)})
	}
	emit(harnesses.EventProgress, "", map[string]any{"phase": "turn_started", "message_id": d.Message.MessageID, "origin": d.Message.Origin})
	res := harnesses.TurnResult{MessageID: d.Message.MessageID}
	finish := func() (harnesses.TurnResult, error) {
		res.Duration = time.Since(start)
		if st, msg := harnesses.StatusFor(tctx); st != "" && res.Status != harnesses.TurnCompleted {
			res.Status, res.Error = st, msg
		}
		emit(harnesses.EventCompletion, "", map[string]any{"status": res.Status, "error": res.Error, "session_id": res.SessionID, "duration_ms": res.Duration.Milliseconds()})
		return res, nil
	}

	a.mu.Lock()
	if a.proc == nil || !a.proc.alive() {
		err = a.startLocked(tctx)
	}
	p, recovery := a.proc, a.recovery
	a.mu.Unlock()
	if err != nil {
		res.Status, res.Error = harnesses.TurnFailed, err.Error()
		return finish()
	}

	events, unsubscribe := p.subscribe()
	defer unsubscribe()
	r, err := p.call(tctx, map[string]any{"type": "prompt", "message": a.prompt(d, recovery)})
	if err != nil {
		res.Status, res.Error = harnesses.TurnFailed, "prompt: "+err.Error()
		return finish()
	}
	var disp struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(r.Data, &disp)
	if disp.Disposition == "handled" {
		// Handled without a model turn; no agent_settled follows.
		res.Status = harnesses.TurnCompleted
		return finish()
	}

	var output, stopReason, errMsg string
	interrupted := false
	var abortDeadline <-chan time.Time
	done := tctx.Done()
	for {
		select {
		case <-done:
			done = nil
			interrupted = true
			_ = p.send(map[string]any{"type": "abort"})
			emit(harnesses.EventProgress, "", map[string]any{"phase": "abort_sent"})
			abortDeadline = time.After(a.grace)
		case <-abortDeadline:
			a.mu.Lock()
			a.killLocked()
			a.mu.Unlock()
			res.Status, res.Error = harnesses.TurnInterrupted, "abort not acknowledged; pi restarted"
			return finish()
		case <-p.done:
			a.mu.Lock()
			stopping := a.stopping
			a.mu.Unlock()
			res.Status, res.Error = harnesses.TurnFailed, "pi exited"+stderrNote(p)
			if interrupted || stopping {
				res.Status, res.Error = harnesses.TurnInterrupted, "stopped"
			}
			return finish()
		case rec := <-events:
			var ev event
			_ = json.Unmarshal(rec.raw, &ev)
			switch ev.Type {
			case "tool_execution_start":
				emit(harnesses.EventToolRequest, ev.ToolCallID, map[string]any{"name": ev.ToolName, "input": json.RawMessage(nonEmpty(ev.Args))})
			case "tool_execution_end":
				emit(harnesses.EventToolResult, ev.ToolCallID, map[string]any{"name": ev.ToolName, "is_error": ev.IsError, "content": json.RawMessage(nonEmpty(ev.Result))})
			case "message_end":
				if ev.Message == nil || ev.Message.Role != "assistant" {
					continue
				}
				stopReason, errMsg = ev.Message.StopReason, ev.Message.ErrorMessage
				var text strings.Builder
				for _, c := range ev.Message.Content {
					if c.Type == "text" {
						text.WriteString(c.Text)
					}
				}
				if text.Len() > 0 {
					output = text.String()
					emit(harnesses.EventOutput, "", map[string]any{"text": harnesses.TruncateUTF8(output, 8192)})
				}
				if stopReason == "error" {
					emit(harnesses.EventError, "", map[string]any{"error": errMsg})
				}
			case "auto_retry_start", "compaction_start", "extension_error":
				emit(harnesses.EventProgress, "", map[string]any{"phase": ev.Type, "detail": json.RawMessage(rec.raw)})
			case "agent_settled":
				res.Output = output
				switch {
				case interrupted || stopReason == "aborted":
					res.Status, res.Error = harnesses.TurnInterrupted, "interrupted"
				case stopReason == "error":
					res.Status, res.Error = harnesses.TurnFailed, errMsg
				default:
					res.Status = harnesses.TurnCompleted
				}
				// The session file exists once the turn has content.
				var state struct {
					SessionFile string `json:"sessionFile"`
					SessionID   string `json:"sessionId"`
				}
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if st, err := p.call(sctx, map[string]any{"type": "get_state"}); err == nil {
					_ = json.Unmarshal(st.Data, &state)
				}
				cancel()
				a.mu.Lock()
				if res.Status == harnesses.TurnCompleted {
					a.session.Turns++
				}
				if state.SessionFile != "" {
					a.session.SessionFile, a.session.SessionID = state.SessionFile, state.SessionID
				}
				res.SessionID = a.session.SessionID
				if recovery != nil {
					res.Recovery = &harnesses.ResumeResult{Mode: recovery.Mode, Note: recovery.Note, SessionID: a.session.SessionID}
					a.recovery = nil
				}
				if err := a.saveSessionLocked(); err != nil && res.Status == harnesses.TurnCompleted {
					res.Status, res.Error = harnesses.TurnFailed, err.Error()
				}
				a.mu.Unlock()
				return finish()
			}
		}
	}
}

func nonEmpty(b json.RawMessage) []byte {
	if len(bytes.TrimSpace(b)) == 0 {
		return []byte("null")
	}
	return b
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// tailBuffer keeps the last 32 KiB written.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 32<<10 {
		keep := append([]byte(nil), t.buf.Bytes()[t.buf.Len()-32<<10:]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
