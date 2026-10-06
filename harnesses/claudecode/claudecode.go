// Package claudecode is the Claude Code harness adapter (design §7, ADR-0001).
//
// Each delivered message runs one unattended `claude -p` process:
//
//	claude -p --output-format stream-json --verbose [--bare]
//	       [--resume <session_id>] --mcp-config <runner>/claude/mcp.json --strict-mcp-config
//	       --settings <runner>/claude/settings.json --append-system-prompt <bootstrap>
//	       --permission-mode bypassPermissions --permission-prompts none [--model <m>]
//
// with the prompt (envelope + body) on stdin, HOME=/seat/home and
// cwd=/seat/workspace. Platform tools arrive through `steadmesh-tools mcp`.
// The model is reached only through the platform model proxy
// (ANTHROPIC_BASE_URL=<platform>/v1/model/<connection>); the seat's projected
// token authenticates it, via an apiKeyHelper that re-reads the token file
// (sent as both Authorization: Bearer and X-Api-Key) or, as a fallback,
// ANTHROPIC_AUTH_TOKEN (Authorization: Bearer only). No model credential
// enters the sandbox.
//
// Checkpoint = session id + claude version (application_checkpoint). When a
// session cannot be resumed, the adapter starts a new session seeded with the
// portable handoff and reports a recovery note; it never claims byte-for-byte
// continuation (§6.4).
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	AdapterName = "claude-code"
	// PinnedVersion is the Claude Code version the seat image installs and the
	// feasibility spike verified (docs/decisions.html#harness).
	PinnedVersion = "2.1.289"
	// SessionFormatPrefix prefixes Checkpoint.FormatVersion.
	SessionFormatPrefix = "claude-code-session/"
	// MCPServerName is the server key in mcp.json; tools appear to the model
	// as mcp__steadmesh__<tool>.
	MCPServerName = "steadmesh"

	defaultInterruptGrace = 10 * time.Second
	stderrTail            = 32 << 10
)

// Harness profile config keys (HarnessProfile.Config).
const (
	ConfigBinary         = "claude_bin"      // default "claude"
	ConfigVersion        = "version"         // when set, Prepare requires exactly this version
	ConfigPermissionMode = "permission_mode" // default bypassPermissions
	ConfigBare           = "bare"            // default "true"
	ConfigAuth           = "auth"            // api_key_helper (default) or auth_token
	ConfigAllowedTools   = "allowed_tools"   // optional --allowedTools value
	ConfigPromptSnapshot = "system_prompt_snapshot"
	ConfigInterruptGrace = "interrupt_grace" // Go duration, default 10s
	ConfigMaxRetries     = "max_retries"     // CLAUDE_CODE_MAX_RETRIES, default 4
)

// SessionState is <runner>/claude/session.json.
type SessionState struct {
	SessionID      string    `json:"session_id"`
	ClaudeVersion  string    `json:"claude_version"`
	ConfigRevision string    `json:"config_revision,omitempty"`
	Turns          int       `json:"turns"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Adapter runs Claude Code.
type Adapter struct {
	mu        sync.Mutex
	env       harnesses.Environment
	bin       string
	version   string
	cfg       config
	bus       *harnesses.EventBus
	guard     harnesses.TurnGuard
	started   bool
	session   SessionState
	recovery  *harnesses.ResumeResult // pending: seed the next new session with the handoff
	handoff   *runtimeapi.Handoff
	proc      *os.Process
	killNow   bool
	stateDir  string
	bootstrap string

	// Tap, when set before Prepare, receives every raw stream-json line
	// (diagnostics and recording test fixtures).
	Tap func(line []byte)
}

type config struct {
	permissionMode string
	bare           bool
	auth           string
	allowedTools   string
	promptSnapshot string
	grace          time.Duration
	maxRetries     int
}

// New returns a Claude Code adapter.
func New() *Adapter { return &Adapter{bus: harnesses.NewEventBus(4096)} }

var _ harnesses.Adapter = (*Adapter)(nil)

func (a *Adapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{
		Adapter:              AdapterName,
		Version:              PinnedVersion,
		ToolTransports:       []string{"mcp", "cli"},
		EventStreaming:       true,
		Interruption:         "signal",
		RecoveryModes:        []string{harnesses.RecoveryFresh, harnesses.RecoveryNativeResume, harnesses.RecoveryPortableHandoff},
		CheckpointGuarantees: []string{harnesses.GuaranteeApplication},
		GracefulSuspension:   false,
		SessionFormat:        SessionFormatPrefix + PinnedVersion,
	}
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func parseConfig(m map[string]string) (config, error) {
	c := config{permissionMode: "bypassPermissions", bare: true, auth: "api_key_helper", promptSnapshot: "off", grace: defaultInterruptGrace, maxRetries: 4}
	if v := m[ConfigPermissionMode]; v != "" {
		c.permissionMode = v
	}
	if v := m[ConfigBare]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("claudecode: %s: %w", ConfigBare, err)
		}
		c.bare = b
	}
	if v := m[ConfigAuth]; v != "" {
		if v != "api_key_helper" && v != "auth_token" {
			return c, fmt.Errorf("claudecode: %s must be api_key_helper or auth_token", ConfigAuth)
		}
		c.auth = v
	}
	c.allowedTools = m[ConfigAllowedTools]
	if v := m[ConfigMaxRetries]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("claudecode: %s must be a non-negative integer", ConfigMaxRetries)
		}
		c.maxRetries = n
	}
	if v, ok := m[ConfigPromptSnapshot]; ok {
		c.promptSnapshot = v
	}
	if v := m[ConfigInterruptGrace]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("claudecode: %s: %w", ConfigInterruptGrace, err)
		}
		c.grace = d
	}
	// The sandbox is the boundary, and an unattended seat must never wait on a
	// permission prompt (§11.1).
	switch c.permissionMode {
	case "bypassPermissions", "dontAsk", "acceptEdits", "auto":
	default:
		return c, fmt.Errorf("claudecode: permission mode %q can prompt; use bypassPermissions or dontAsk", c.permissionMode)
	}
	return c, nil
}

func (a *Adapter) Prepare(ctx context.Context, env harnesses.Environment) error {
	cfg, err := parseConfig(env.HarnessConfig)
	if err != nil {
		return err
	}
	if env.ModelProxyURL == "" {
		return errors.New("claudecode: a model connection (model proxy URL) is required")
	}
	if env.ToolCommand == "" || env.TokenFile == "" {
		return errors.New("claudecode: ToolCommand and TokenFile are required")
	}
	for _, d := range []string{env.WorkspaceDir, env.HomeDir, env.RunnerDir} {
		if d == "" {
			return errors.New("claudecode: workspace, home and runner dirs are required")
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("claudecode: %w", err)
		}
	}
	bin := env.HarnessConfig[ConfigBinary]
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("claudecode: claude binary: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, "--version")
	cmd.Env = a.baseEnv(env)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("claudecode: claude --version: %w", err)
	}
	version := versionRe.FindString(string(out))
	if version == "" {
		return fmt.Errorf("claudecode: cannot parse version from %q", strings.TrimSpace(string(out)))
	}
	if want := env.HarnessConfig[ConfigVersion]; want != "" && want != version {
		return fmt.Errorf("claudecode: pinned version %s required, found %s", want, version)
	}

	stateDir := filepath.Join(env.RunnerDir, "claude")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	if err := writeJSON(filepath.Join(stateDir, "mcp.json"), mcpConfig(env)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(stateDir, "settings.json"), settings(env, cfg)); err != nil {
		return err
	}
	a.mu.Lock()
	a.env, a.bin, a.version, a.cfg, a.stateDir = env, path, version, cfg, stateDir
	a.mu.Unlock()
	return nil
}

// mcpConfig runs steadmesh-tools as a stdio MCP server with the seat environment.
func mcpConfig(env harnesses.Environment) map[string]any {
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
	return map[string]any{"mcpServers": map[string]any{
		MCPServerName: map[string]any{"type": "stdio", "command": env.ToolCommand, "args": []string{"mcp"}, "env": e},
	}}
}

func settings(env harnesses.Environment, cfg config) map[string]any {
	// Only well-known keys: with -p, a settings file that fails validation is
	// silently ignored, which would drop the apiKeyHelper.
	s := map[string]any{
		"permissions": map[string]any{"defaultMode": cfg.permissionMode},
		// Seats live for months; the default 30-day cleanup would delete the
		// transcript that --resume needs.
		"cleanupPeriodDays": 3650,
	}
	if cfg.auth == "api_key_helper" {
		// Re-read on every helper invocation: projected tokens rotate.
		s["apiKeyHelper"] = "cat " + shellQuote(env.TokenFile)
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writeJSON(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("claudecode: write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// baseEnv is the allow-listed environment for claude. Nothing is inherited
// implicitly, so no ANTHROPIC_* credential from the runner's environment can leak.
func (a *Adapter) baseEnv(env harnesses.Environment) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	out := []string{
		"PATH=" + path,
		"HOME=" + env.HomeDir,
		"LANG=C.UTF-8",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS=300000",
	}
	if env.TmpDir != "" {
		out = append(out, "TMPDIR="+env.TmpDir)
	}
	if env.ModelProxyURL != "" {
		out = append(out, "ANTHROPIC_BASE_URL="+env.ModelProxyURL)
	}
	for _, kv := range env.ExtraEnv {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "STEADMESH_") {
			out = append(out, kv)
		}
	}
	return out
}

func (a *Adapter) sessionPath() string { return filepath.Join(a.stateDir, "session.json") }

func (a *Adapter) loadSession() (SessionState, bool) {
	b, err := os.ReadFile(a.sessionPath())
	if err != nil {
		return SessionState{}, false
	}
	var s SessionState
	if json.Unmarshal(b, &s) != nil || s.SessionID == "" {
		return SessionState{}, false
	}
	return s, true
}

func (a *Adapter) saveSessionLocked() error {
	a.session.UpdatedAt = time.Now().UTC()
	return writeJSON(a.sessionPath(), a.session)
}

// transcriptExists reports whether Claude Code's transcript for id exists
// under HOME/.claude/projects/*/<id>.jsonl.
func (a *Adapter) transcriptExists(id string) bool {
	if id == "" || strings.ContainsAny(id, `/\*?[`) {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(a.env.HomeDir, ".claude", "projects", "*", id+".jsonl"))
	return len(m) > 0
}

func (a *Adapter) StartOrResume(ctx context.Context, rec harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bin == "" {
		return harnesses.ResumeResult{}, errors.New("claudecode: Prepare has not been called")
	}
	a.started = true
	a.handoff = rec.Handoff
	a.bootstrap = harnesses.RenderBootstrap(a.env, "")
	fallback := func(note string) (harnesses.ResumeResult, error) {
		a.session = SessionState{ClaudeVersion: a.version, ConfigRevision: a.env.ConfigRevision}
		_ = os.Remove(a.sessionPath())
		r := harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
		a.recovery = &r
		return r, nil
	}

	if s, ok := a.loadSession(); ok {
		if a.transcriptExists(s.SessionID) {
			a.session = s
			res := harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: s.SessionID}
			if s.ClaudeVersion != "" && s.ClaudeVersion != a.version {
				res.Note = fmt.Sprintf("session written by Claude Code %s; resuming with %s", s.ClaudeVersion, a.version)
			}
			return res, nil
		}
		return fallback(fmt.Sprintf("Claude Code session %s is missing from the workspace volume; started a new session from the portable handoff (not a continuation of the previous native session)", s.SessionID))
	}
	if rec.Session != nil {
		if rec.Session.HarnessAdapter != AdapterName {
			return fallback(fmt.Sprintf("native session from adapter %q (format %s) cannot be converted to Claude Code; started a new session from the portable handoff", rec.Session.HarnessAdapter, rec.Session.FormatVersion))
		}
		if a.transcriptExists(rec.Session.CheckpointRef) {
			a.session = SessionState{SessionID: rec.Session.CheckpointRef, ClaudeVersion: strings.TrimPrefix(rec.Session.FormatVersion, SessionFormatPrefix), ConfigRevision: a.env.ConfigRevision}
			if err := a.saveSessionLocked(); err != nil {
				return harnesses.ResumeResult{}, err
			}
			return harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: a.session.SessionID}, nil
		}
		return fallback(fmt.Sprintf("Claude Code session %s recorded by the platform is not present on this volume; started a new session from the portable handoff (not a continuation of the previous native session)", rec.Session.CheckpointRef))
	}
	a.session = SessionState{ClaudeVersion: a.version, ConfigRevision: a.env.ConfigRevision}
	return harnesses.ResumeResult{Mode: harnesses.RecoveryFresh}, nil
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
	if ref == "" {
		// No turn has produced a session yet; there is nothing native to resume.
		ref = "none"
	}
	return runtimeapi.Checkpoint{
		HarnessAdapter: AdapterName,
		FormatVersion:  SessionFormatPrefix + a.version,
		CheckpointRef:  ref,
		Guarantee:      harnesses.GuaranteeApplication,
	}, nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	if ctx.Err() != nil {
		a.mu.Lock()
		a.killNow = true
		p := a.proc
		a.mu.Unlock()
		if p != nil {
			killGroup(p.Pid, syscall.SIGKILL)
		}
	}
	done := a.guard.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(a.cfg.grace + 5*time.Second):
		}
	}
	a.bus.Close()
	return nil
}

// ---- turns ----------------------------------------------------------------------

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

	a.mu.Lock()
	sessionID := a.session.SessionID
	recovery := a.recovery
	a.mu.Unlock()

	res := harnesses.TurnResult{MessageID: d.Message.MessageID}
	run, err := a.runOnce(tctx, d, sessionID, recovery, emit)
	if err == nil && sessionID != "" && run.parser.ResumeFailed() {
		note := fmt.Sprintf("Claude Code could not resume session %s (%s); started a new session from the portable handoff (not a continuation of the previous native session)", sessionID, strings.Join(run.parser.Result.Errors, "; "))
		emit(harnesses.EventProgress, "", map[string]any{"phase": "resume_failed", "session_id": sessionID, "note": note})
		recovery = &harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
		a.mu.Lock()
		a.session.SessionID = ""
		a.recovery = recovery
		_ = os.Remove(a.sessionPath())
		a.mu.Unlock()
		run, err = a.runOnce(tctx, d, "", recovery, emit)
	}

	res.Duration = time.Since(start)
	switch {
	case err != nil:
		res.Status, res.Error = harnesses.TurnFailed, err.Error()
	default:
		res.SessionID = run.parser.SessionID
		res.Output = run.parser.LastText
		if r := run.parser.Result; r != nil {
			res.Output = r.Result
		}
		res.Status, res.Error = run.status()
	}
	if st, msg := harnesses.StatusFor(tctx); st != "" {
		res.Status, res.Error = st, msg
	}
	if recovery != nil && res.SessionID != "" {
		res.Recovery = &harnesses.ResumeResult{Mode: recovery.Mode, Note: recovery.Note, SessionID: res.SessionID}
	}

	a.mu.Lock()
	if res.SessionID != "" && (res.Status == harnesses.TurnCompleted || run.parser.AssistantMessages > 0) {
		// A session that produced output exists on disk and can be resumed,
		// even if the turn was interrupted or failed later.
		a.session.SessionID = res.SessionID
		a.session.ClaudeVersion = a.version
		a.session.ConfigRevision = a.env.ConfigRevision
		if res.Status == harnesses.TurnCompleted {
			a.session.Turns++
		}
		if err := a.saveSessionLocked(); err != nil && res.Status == harnesses.TurnCompleted {
			res.Status, res.Error = harnesses.TurnFailed, err.Error()
		}
		a.recovery = nil
	}
	a.mu.Unlock()

	data := map[string]any{"status": res.Status, "error": res.Error, "session_id": res.SessionID, "duration_ms": res.Duration.Milliseconds()}
	if run != nil && run.parser.Result != nil {
		r := run.parser.Result
		data["result_subtype"], data["num_turns"], data["total_cost_usd"] = r.Subtype, r.NumTurns, r.TotalCostUSD
	}
	if run != nil {
		data["exit_code"] = run.exitCode
	}
	emit(harnesses.EventCompletion, "", data)
	return res, nil
}

type runResult struct {
	parser   *StreamParser
	exitCode int
	waitErr  error
	stderr   string
}

func (r *runResult) status() (string, string) {
	if p := r.parser.Result; p != nil {
		if p.IsError {
			msg := strings.Join(p.Errors, "; ")
			if msg == "" {
				msg = harnesses.TruncateUTF8(p.Result, 2048)
				if p.Subtype != "" && p.Subtype != "success" {
					msg = p.Subtype + ": " + msg
				}
			}
			return harnesses.TurnFailed, msg
		}
		return harnesses.TurnCompleted, ""
	}
	msg := fmt.Sprintf("claude exited (code %d) without a result", r.exitCode)
	if r.stderr != "" {
		msg += ": " + harnesses.TruncateUTF8(lastLines(r.stderr, 5), 2048)
	}
	return harnesses.TurnFailed, msg
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Args builds the claude command line for one turn.
func (a *Adapter) Args(sessionID, systemPrompt string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if a.cfg.bare {
		args = append(args, "--bare")
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	args = append(args,
		"--mcp-config", filepath.Join(a.stateDir, "mcp.json"), "--strict-mcp-config",
		"--settings", filepath.Join(a.stateDir, "settings.json"),
		"--permission-mode", a.cfg.permissionMode,
		"--permission-prompts", "none",
		"--append-system-prompt", systemPrompt,
	)
	if a.cfg.promptSnapshot != "" {
		args = append(args, "--system-prompt-snapshot", a.cfg.promptSnapshot)
	}
	if a.cfg.allowedTools != "" {
		args = append(args, "--allowedTools", a.cfg.allowedTools)
	}
	if a.env.Model != "" {
		args = append(args, "--model", a.env.Model)
	}
	return args
}

func (a *Adapter) prompt(d harnesses.Delivery, recovery *harnesses.ResumeResult, fresh bool) string {
	var sb strings.Builder
	if recovery != nil && fresh {
		sb.WriteString("<recovery>\n")
		sb.WriteString(recovery.Note)
		sb.WriteString("\n")
		if a.handoff != nil {
			sb.WriteString(harnesses.RenderHandoff(*a.handoff))
		}
		sb.WriteString("Use memory.search, messages.history and operations.get to rebuild context before acting.\n</recovery>\n\n")
	}
	sb.WriteString(harnesses.RenderEnvelope(d))
	return sb.String()
}

func (a *Adapter) runOnce(ctx context.Context, d harnesses.Delivery, sessionID string, recovery *harnesses.ResumeResult, emit func(string, string, any)) (*runResult, error) {
	a.mu.Lock()
	env := a.env
	sys := a.bootstrap
	cfg := a.cfg
	a.mu.Unlock()

	cmd := exec.Command(a.bin, a.Args(sessionID, sys)...)
	cmd.Dir = env.WorkspaceDir
	cmd.Env = a.baseEnv(env)
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_MAX_RETRIES="+strconv.Itoa(cfg.maxRetries))
	cmd.Env = append(cmd.Env, "ANTHROPIC_CUSTOM_HEADERS="+fmt.Sprintf("%s: %d\n%s: %s", runtimeapi.HeaderGeneration, env.Generation, runtimeapi.HeaderExecution, d.ExecutionID))
	cmd.Env = append(cmd.Env, "STEADMESH_EXECUTION="+d.ExecutionID)
	if cfg.auth == "auth_token" {
		tok, err := os.ReadFile(env.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token: %w", err)
		}
		cmd.Env = append(cmd.Env, "ANTHROPIC_AUTH_TOKEN="+strings.TrimSpace(string(tok)))
	}
	cmd.Stdin = strings.NewReader(a.prompt(d, recovery, sessionID == ""))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	a.mu.Lock()
	a.proc = cmd.Process
	killNow := a.killNow
	a.mu.Unlock()
	if killNow {
		killGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	emit(harnesses.EventProgress, "", map[string]any{"phase": "process_started", "pid": cmd.Process.Pid, "resume": sessionID})

	// Interrupt: SIGINT to the process group, then SIGKILL after the grace period.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-stopWatch:
			return
		case <-ctx.Done():
		}
		a.mu.Lock()
		immediate := a.killNow
		a.mu.Unlock()
		if immediate {
			killGroup(cmd.Process.Pid, syscall.SIGKILL)
			return
		}
		killGroup(cmd.Process.Pid, syscall.SIGINT)
		emit(harnesses.EventProgress, "", map[string]any{"phase": "interrupt_sent", "signal": "SIGINT"})
		select {
		case <-stopWatch:
		case <-time.After(cfg.grace):
			killGroup(cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	p := &StreamParser{}
	_ = readLines(stdout, func(l []byte) {
		if a.Tap != nil {
			a.Tap(append([]byte(nil), l...))
		}
		hadInit := p.Init != nil
		for _, ev := range p.Parse(l) {
			emit(ev.Kind, ev.CorrelationID, ev.Data)
		}
		if !hadInit && p.Init != nil {
			if cfg.auth == "api_key_helper" && p.Init.APIKeySource != "" && p.Init.APIKeySource != "apiKeyHelper" {
				emit(harnesses.EventError, "", map[string]any{"error": "settings not applied: apiKeySource is " + p.Init.APIKeySource + ", expected apiKeyHelper"})
			}
			if st := mcpStatus(p.Init); st != "connected" {
				emit(harnesses.EventError, "", map[string]any{"error": "steadmesh MCP server status: " + st})
			}
		}
	})
	werr := cmd.Wait()
	close(stopWatch)
	<-watchDone
	// Reap stragglers (e.g. the MCP server) left in the process group.
	killGroup(cmd.Process.Pid, syscall.SIGKILL)
	a.mu.Lock()
	a.proc = nil
	a.mu.Unlock()
	rr := &runResult{parser: p, waitErr: werr, stderr: stderr.String(), exitCode: cmd.ProcessState.ExitCode()}
	return rr, nil
}

func mcpStatus(in *Init) string {
	for _, s := range in.MCPServers {
		if s.Name == MCPServerName {
			return s.Status
		}
	}
	return "absent"
}

func killGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, sig)
}

// tailBuffer keeps the last stderrTail bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > stderrTail {
		b := t.buf.Bytes()
		keep := append([]byte(nil), b[len(b)-stderrTail:]...)
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
