// Package codex is the Codex harness adapter. It drives one long-running
// `codex app-server` process over JSON-RPC on stdio (CONTRACT.md):
//
//   - CODEX_HOME is <runner>/codex on the seat volume, so threads (rollouts
//     and state databases) survive Pod replacement and resume natively;
//   - config.toml points a custom provider at the seat's model forwarder
//     (wire_api "responses", the only API Codex speaks) with a placeholder
//     key, and adds steadmesh-tools as a required stdio MCP server whose
//     tools are approved without prompting;
//   - threads run with approval policy "never" and danger-full-access: the
//     seat Pod is the boundary;
//   - a turn is turn/start ... turn/completed; interruption is turn/interrupt.
//
// Checkpoint = thread id + Codex version (application_checkpoint). A thread
// that cannot be resumed is replaced by a new one seeded with the portable
// handoff, and the turn reports it.
package codex

import (
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/skills"
)

const (
	// AdapterName is the harness profile adapter value.
	AdapterName = "codex"
	// PinnedVersion is the verified Codex release (CONTRACT.md).
	PinnedVersion = "0.160.1"
	// SessionFormatPrefix prefixes Checkpoint.FormatVersion.
	SessionFormatPrefix = "codex-thread/"
	// MCPServerName is the tool bridge's server name; Codex presents its
	// tools in the Responses namespace mcp__steadmesh.
	MCPServerName = "steadmesh"
	// ProviderID is the custom model provider; built-in IDs are reserved.
	ProviderID = "steadmesh"

	// SettingReasoningEffort is passed as turn/start effort.
	SettingReasoningEffort = "reasoning_effort"

	defaultInterruptGrace = 15 * time.Second
	startTimeout          = 60 * time.Second
)

// Harness profile config keys.
const (
	ConfigBinary         = "codex_bin"       // default "codex"
	ConfigVersion        = "version"         // when set, Prepare requires exactly this version
	ConfigInterruptGrace = "interrupt_grace" // Go duration, default 15s
)

func init() {
	harnesses.Register(harnesses.Descriptor{
		Name:       AdapterName,
		APIs:       []string{harnesses.APIOpenAIResponses},
		NeedsModel: true,
		Settings: map[string]harnesses.Setting{
			SettingReasoningEffort: {Description: "reasoning effort sent with each turn (turn/start effort)"},
		},
		Capabilities: []string{"tools", "mcp", "event_stream", "session_resume", "interrupt", "application_checkpoint"},
		New:          func() harnesses.Adapter { return New() },
	})
}

// SessionState is <runner>/codex-state.json.
type SessionState struct {
	ThreadID       string    `json:"thread_id"`
	CodexVersion   string    `json:"codex_version"`
	ConfigRevision string    `json:"config_revision,omitempty"`
	Turns          int       `json:"turns"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Adapter runs Codex.
type Adapter struct {
	mu        sync.Mutex
	env       harnesses.Environment
	bin       string
	version   string
	home      string
	grace     time.Duration
	bus       *harnesses.EventBus
	guard     harnesses.TurnGuard
	started   bool
	session   SessionState
	recovery  *harnesses.ResumeResult
	handoff   *runtimeapi.Handoff
	bootstrap string

	proc *exec.Cmd
	rpc  *rpcConn
	// stopping is set by Stop; a turn cut short by it is interrupted.
	stopping bool
	// procDone is closed when the app-server exits.
	procDone chan struct{}
	stderr   *tailBuffer
	// The snapshot belongs only to this adapter. The server's skill roots are
	// configured before starting or resuming a thread. Changed skills replace
	// the process at the next turn boundary without replacing the native thread.
	skillSnapshot *skills.Snapshot

	// Tap, when set before StartOrResume, receives every raw protocol line.
	Tap func(line []byte)
}

// New returns a Codex adapter.
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
		return errors.New("codex: a model is required")
	}
	if env.Model.API != harnesses.APIOpenAIResponses {
		return fmt.Errorf("codex: Codex speaks only %s, not %q", harnesses.APIOpenAIResponses, env.Model.API)
	}
	if env.ToolCommand == "" || env.RunnerDir == "" || env.WorkspaceDir == "" || env.HomeDir == "" {
		return errors.New("codex: ToolCommand, workspace, home and runner dirs are required")
	}
	grace := defaultInterruptGrace
	if v := env.HarnessConfig[ConfigInterruptGrace]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("codex: %s: %w", ConfigInterruptGrace, err)
		}
		grace = d
	}
	for _, d := range []string{env.WorkspaceDir, env.HomeDir, env.RunnerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("codex: %w", err)
		}
	}
	bin := env.HarnessConfig[ConfigBinary]
	if bin == "" {
		bin = "codex"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("codex: codex binary: %w", err)
	}
	home := filepath.Join(env.RunnerDir, "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, "--version")
	cmd.Env = baseEnv(env, home)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("codex: codex --version: %w", err)
	}
	version := versionRe.FindString(string(out))
	if version == "" {
		return fmt.Errorf("codex: cannot parse version from %q", strings.TrimSpace(string(out)))
	}
	if want := env.HarnessConfig[ConfigVersion]; want != "" && want != version {
		return fmt.Errorf("codex: pinned version %s required, found %s", want, version)
	}
	if err := writeFile(filepath.Join(home, "config.toml"), []byte(ConfigTOML(env))); err != nil {
		return err
	}
	a.mu.Lock()
	a.env, a.bin, a.version, a.home, a.grace = env, path, version, home, grace
	a.mu.Unlock()
	return nil
}

// ConfigTOML renders $CODEX_HOME/config.toml (CONTRACT.md §2, §3, §5).
func ConfigTOML(env harnesses.Environment) string {
	var b strings.Builder
	kv := func(k string, v any) { fmt.Fprintf(&b, "%s = %s\n", k, tomlValue(v)) }
	kv("model", env.Model.ID)
	kv("model_provider", ProviderID)
	kv("approval_policy", "never")
	kv("sandbox_mode", "danger-full-access")
	kv("check_for_update_on_startup", false)
	// Wait for the tool bridge so its tools exist on the first turn.
	kv("mcp_optional_startup_grace_ms", 0)
	b.WriteString("\n[analytics]\n")
	kv("enabled", false)
	b.WriteString("\n[feedback]\n")
	kv("enabled", false)
	b.WriteString("\n[otel]\n")
	kv("metrics_exporter", "none")
	// Otherwise Codex clones github.com/openai/plugins at startup.
	b.WriteString("\n[features]\n")
	kv("plugins", false)
	b.WriteString("\n[model_providers." + ProviderID + "]\n")
	kv("name", "Steadmesh model proxy")
	kv("base_url", strings.TrimSuffix(env.Model.BaseURL, "/")+"/v1")
	kv("wire_api", "responses")
	// The forwarder replaces this with the seat token on every request.
	kv("experimental_bearer_token", env.Model.APIKey)
	b.WriteString("\n[mcp_servers." + MCPServerName + "]\n")
	kv("command", env.ToolCommand)
	kv("args", []string{"mcp"})
	kv("env", toolEnv(env))
	kv("startup_timeout_sec", 30)
	kv("tool_timeout_sec", 600)
	kv("required", true)
	kv("default_tools_approval_mode", "approve")
	for _, s := range env.MCPServers {
		b.WriteString("\n[mcp_servers." + s.Name + "]\n")
		kv("command", s.Command)
		kv("args", s.Args)
		kv("env", s.Env)
		kv("startup_timeout_sec", 60)
		kv("tool_timeout_sec", 600)
		kv("required", true)
		kv("default_tools_approval_mode", "approve")
	}
	return b.String()
}

func toolEnv(env harnesses.Environment) map[string]string {
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
	return e
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		b, _ := json.Marshal(x) // a JSON string is a valid TOML basic string
		return string(b)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = tomlValue(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = tomlValue(k) + " = " + tomlValue(x[k])
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	panic(fmt.Sprintf("codex: no TOML encoding for %T", v))
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("codex: write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// baseEnv is the allow-listed environment for codex: no OPENAI_API_KEY or
// other credential from the runner's environment can leak in.
func baseEnv(env harnesses.Environment, home string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	out := []string{"PATH=" + path, "HOME=" + env.HomeDir, "CODEX_HOME=" + home, "LANG=C.UTF-8", "RUST_LOG=warn"}
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

// ---- process ----------------------------------------------------------------

// startProcess launches app-server and performs the initialize handshake.
// Callers hold a.mu.
func (a *Adapter) startProcessLocked(ctx context.Context) error {
	cmd := exec.Command(a.bin, "app-server")
	cmd.Dir = a.env.WorkspaceDir
	cmd.Env = baseEnv(a.env, a.home)
	harnesses.NewProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	a.stderr = &tailBuffer{}
	cmd.Stderr = a.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex: start app-server: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	a.proc, a.procDone = cmd, done
	a.rpc = newRPC(stdin, stdout, a.answerServerRequest, a.Tap)
	ictx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	if err := a.rpc.call(ictx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "steadmesh", "version": PinnedVersion}}, &init); err != nil {
		a.killLocked()
		return fmt.Errorf("codex: initialize: %w%s", err, a.stderrNote())
	}
	if err := a.rpc.notify("initialized", nil); err != nil {
		a.killLocked()
		return err
	}
	if a.env.LoadSkills != nil {
		if err := a.applySkillsLocked(ictx, a.skillSnapshot); err != nil {
			a.killLocked()
			a.clearSkillsLocked()
			return err
		}
	}
	return nil
}

func (a *Adapter) stderrNote() string {
	if a.stderr == nil {
		return ""
	}
	if s := strings.TrimSpace(a.stderr.String()); s != "" {
		return ": " + harnesses.TruncateUTF8(lastLines(s, 5), 2048)
	}
	return ""
}

func (a *Adapter) killLocked() {
	if a.proc != nil && a.proc.Process != nil {
		harnesses.KillProcessGroup(a.proc.Process.Pid, syscall.SIGKILL)
	}
	if a.procDone != nil {
		select {
		case <-a.procDone:
		case <-time.After(5 * time.Second):
		}
	}
	a.proc, a.rpc, a.procDone = nil, nil, nil
}

// shutdownLocked gives app-server EOF to flush native thread state before a
// skill-set change. The bounded fallback reaps the entire process group.
func (a *Adapter) shutdownLocked(ctx context.Context) {
	if a.rpc != nil && ctx.Err() == nil {
		if stdin, ok := a.rpc.w.(io.Closer); ok {
			_ = stdin.Close()
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-a.procDone:
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	a.killLocked()
}

// clearSkillsLocked removes only generated skill files, never session data.
func (a *Adapter) clearSkillsLocked() {
	if a.skillSnapshot != nil {
		_ = a.skillSnapshot.Close()
		a.skillSnapshot = nil
	}
}

// applySkillsLocked uses the pinned app-server API instead of modifying
// persistent Codex configuration or enabling its native plugin marketplace.
// Replacing with [] is important: a revoked final skill must unload too.
func (a *Adapter) applySkillsLocked(ctx context.Context, snapshot *skills.Snapshot) error {
	roots := []string{}
	if snapshot != nil {
		roots = append(roots, snapshot.SkillDirs...)
	}
	if err := a.rpc.call(ctx, "skills/extraRoots/set", map[string]any{"extraRoots": roots}, nil); err != nil {
		return fmt.Errorf("codex: set skill roots: %w", err)
	}
	var result struct {
		Data []struct {
			Skills []struct {
				Path    string `json:"path"`
				Enabled bool   `json:"enabled"`
			} `json:"skills"`
			Errors []struct {
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"data"`
	}
	if err := a.rpc.call(ctx, "skills/list", map[string]any{"cwds": []string{a.env.WorkspaceDir}, "forceReload": true}, &result); err != nil {
		return fmt.Errorf("codex: reload skills: %w", err)
	}
	loaded := map[string]bool{}
	for _, entry := range result.Data {
		for _, skill := range entry.Skills {
			loaded[filepath.Clean(skill.Path)] = skill.Enabled
		}
	}
	for _, dir := range roots {
		path := filepath.Join(dir, "SKILL.md")
		if !loaded[path] {
			for _, entry := range result.Data {
				for _, issue := range entry.Errors {
					if filepath.Clean(issue.Path) == path {
						return fmt.Errorf("codex: managed skill %s: %s", path, issue.Message)
					}
				}
			}
			return fmt.Errorf("codex: managed skill was not loaded and enabled: %s", path)
		}
	}
	return nil
}

// syncSkillsLocked resolves current authorization before any model turn. An
// unavailable bundle or uncertain reload shuts down the server, so it cannot
// keep using a stale skill cache; the next successful turn resumes its thread.
func (a *Adapter) syncSkillsLocked(ctx context.Context) error {
	if a.env.LoadSkills == nil {
		return nil
	}
	bundle, err := a.env.LoadSkills(ctx)
	var next *skills.Snapshot
	if err == nil {
		next, err = skills.Materialize(filepath.Join(a.env.RunnerDir, "codex-skills"), bundle.Skills)
	}
	if err != nil {
		a.killLocked()
		a.clearSkillsLocked()
		return fmt.Errorf("codex: refresh skills: %w", err)
	}
	if a.skillSnapshot != nil && a.skillSnapshot.Digest == next.Digest {
		return next.Close()
	}
	// extraRoots/set and skills/list are process-level catalog APIs. The
	// pinned contract does not guarantee that an already loaded thread adopts
	// a new skill set. Reinitialize the process and resume the existing native
	// thread so revocations do not depend on an undocumented cache behavior.
	a.shutdownLocked(ctx)
	a.clearSkillsLocked()
	a.skillSnapshot = next
	return nil
}

func (a *Adapter) aliveLocked() bool {
	if a.procDone == nil {
		return false
	}
	select {
	case <-a.procDone:
		return false
	default:
		return true
	}
}

// answerServerRequest declines every server-to-client request. With
// approval policy "never" and full access none should arrive; an unattended
// seat must never wait on one.
func (a *Adapter) answerServerRequest(m rpcMessage) (any, *rpcError) {
	a.bus.Emit(context.Background(), harnesses.Event{Kind: harnesses.EventProgress, Data: harnesses.MustJSON(map[string]any{"phase": "server_request_declined", "method": m.Method})})
	return nil, &rpcError{Code: -32601, Message: "steadmesh seats do not answer " + m.Method}
}

// threadParams are the overrides sent on thread/start and thread/resume.
func (a *Adapter) threadParams() map[string]any {
	return map[string]any{
		"cwd":                   a.env.WorkspaceDir,
		"approvalPolicy":        "never",
		"sandbox":               "danger-full-access",
		"model":                 a.env.Model.ID,
		"modelProvider":         ProviderID,
		"developerInstructions": a.bootstrap,
	}
}

type threadResult struct {
	Thread struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"thread"`
}

// openThreadLocked starts the process if needed and resumes the session's
// thread, or starts a new one when there is none. A thread that cannot be
// resumed becomes a portable-handoff recovery.
func (a *Adapter) openThreadLocked(ctx context.Context) error {
	if !a.aliveLocked() {
		if err := a.startProcessLocked(ctx); err != nil {
			return err
		}
		if a.session.ThreadID != "" {
			p := a.threadParams()
			p["threadId"] = a.session.ThreadID
			p["excludeTurns"] = true
			var res threadResult
			err := a.rpc.call(ctx, "thread/resume", p, &res)
			if err == nil {
				return nil
			}
			var rerr *rpcError
			if !errors.As(err, &rerr) {
				return fmt.Errorf("codex: thread/resume: %w", err)
			}
			note := fmt.Sprintf("Codex could not resume thread %s (%s); started a new thread from the portable handoff (not a continuation of the previous native session)", a.session.ThreadID, rerr.Message)
			a.recovery = &harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
			a.session.ThreadID = ""
		}
	}
	if a.session.ThreadID != "" {
		return nil
	}
	var res threadResult
	if err := a.rpc.call(ctx, "thread/start", a.threadParams(), &res); err != nil {
		return fmt.Errorf("codex: thread/start: %w%s", err, a.stderrNote())
	}
	a.session.ThreadID = res.Thread.ID
	a.session.CodexVersion = a.version
	a.session.ConfigRevision = a.env.ConfigRevision
	return a.saveSessionLocked()
}

// ---- state ------------------------------------------------------------------

func (a *Adapter) sessionPath() string { return filepath.Join(a.env.RunnerDir, "codex-state.json") }

func (a *Adapter) loadSession() (SessionState, bool) {
	b, err := os.ReadFile(a.sessionPath())
	if err != nil {
		return SessionState{}, false
	}
	var s SessionState
	if json.Unmarshal(b, &s) != nil || s.ThreadID == "" {
		return SessionState{}, false
	}
	return s, true
}

func (a *Adapter) saveSessionLocked() error {
	a.session.UpdatedAt = time.Now().UTC()
	b, _ := json.MarshalIndent(a.session, "", "  ")
	return writeFile(a.sessionPath(), b)
}

// rolloutExists reports whether the thread's rollout is on the volume.
func (a *Adapter) rolloutExists(id string) bool {
	if id == "" || strings.ContainsAny(id, `/\*?[`) {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(a.home, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	return len(m) > 0
}

func (a *Adapter) StartOrResume(ctx context.Context, rec harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bin == "" {
		return harnesses.ResumeResult{}, errors.New("codex: Prepare has not been called")
	}
	a.started = true
	a.handoff = rec.Handoff
	a.bootstrap = harnesses.RenderBootstrap(a.env, "")
	fallback := func(note string) (harnesses.ResumeResult, error) {
		a.session = SessionState{CodexVersion: a.version, ConfigRevision: a.env.ConfigRevision}
		_ = os.Remove(a.sessionPath())
		r := harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
		a.recovery = &r
		return r, nil
	}
	res := harnesses.ResumeResult{Mode: harnesses.RecoveryFresh}
	switch s, ok := a.loadSession(); {
	case ok && a.rolloutExists(s.ThreadID):
		a.session = s
		res = harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: s.ThreadID}
		if s.CodexVersion != "" && s.CodexVersion != a.version {
			res.Note = fmt.Sprintf("thread written by Codex %s; resuming with %s", s.CodexVersion, a.version)
		}
	case ok:
		return fallback(fmt.Sprintf("Codex thread %s is missing from the seat volume; started a new thread from the portable handoff (not a continuation of the previous native session)", s.ThreadID))
	case rec.Session != nil && rec.Session.HarnessAdapter != AdapterName:
		return fallback(fmt.Sprintf("native session from adapter %q (format %s) cannot be converted to Codex; started a new thread from the portable handoff", rec.Session.HarnessAdapter, rec.Session.FormatVersion))
	case rec.Session != nil && rec.Session.CheckpointRef != "none" && a.rolloutExists(rec.Session.CheckpointRef):
		a.session = SessionState{ThreadID: rec.Session.CheckpointRef, CodexVersion: strings.TrimPrefix(rec.Session.FormatVersion, SessionFormatPrefix), ConfigRevision: a.env.ConfigRevision}
		if err := a.saveSessionLocked(); err != nil {
			return harnesses.ResumeResult{}, err
		}
		res = harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: a.session.ThreadID}
	case rec.Session != nil && rec.Session.CheckpointRef != "none":
		return fallback(fmt.Sprintf("Codex thread %s recorded by the platform is not on this volume; started a new thread from the portable handoff (not a continuation of the previous native session)", rec.Session.CheckpointRef))
	default:
		a.session = SessionState{CodexVersion: a.version, ConfigRevision: a.env.ConfigRevision}
	}
	// Start the app-server now so a broken installation fails at start.
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := a.syncSkillsLocked(sctx); err != nil {
		return harnesses.ResumeResult{}, err
	}
	if err := a.openThreadLocked(sctx); err != nil {
		return harnesses.ResumeResult{}, err
	}
	if a.recovery != nil && res.Mode == harnesses.RecoveryNativeResume {
		res = *a.recovery
	}
	return res, nil
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
	ref := a.session.ThreadID
	if ref == "" {
		ref = "none"
	}
	return runtimeapi.Checkpoint{HarnessAdapter: AdapterName, FormatVersion: SessionFormatPrefix + a.version, CheckpointRef: ref, Guarantee: harnesses.GuaranteeApplication}, nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	// Cancel an in-flight bundle fetch or reload before taking mu: both are
	// performed at a guarded turn boundary while holding that mutex.
	done := a.guard.Stop()
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	if ctx.Err() != nil {
		a.mu.Lock()
		a.killLocked()
		a.mu.Unlock()
	}
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(a.grace + 5*time.Second):
		}
	}
	a.mu.Lock()
	a.killLocked()
	a.clearSkillsLocked()
	a.mu.Unlock()
	a.bus.Close()
	return nil
}

// ---- turns ------------------------------------------------------------------

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

type turnInfo struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
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
	err = a.syncSkillsLocked(tctx)
	if err == nil {
		err = a.openThreadLocked(tctx)
	}
	recovery := a.recovery
	threadID := a.session.ThreadID
	rpc := a.rpc
	a.mu.Unlock()
	if err != nil {
		res.Status, res.Error = harnesses.TurnFailed, err.Error()
		return finish()
	}
	res.SessionID = threadID

	notes, unsubscribe := rpc.subscribe()
	defer unsubscribe()
	params := map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": a.prompt(d, recovery)}}}
	if e := a.env.Model.Settings[SettingReasoningEffort]; e != "" {
		params["effort"] = e
	}
	var accepted struct {
		Turn turnInfo `json:"turn"`
	}
	if err := rpc.call(tctx, "turn/start", params, &accepted); err != nil {
		res.Status, res.Error = harnesses.TurnFailed, "turn/start: "+err.Error()
		return finish()
	}
	turnID := accepted.Turn.ID
	emit(harnesses.EventProgress, "", map[string]any{"phase": "turn_accepted", "thread_id": threadID, "turn_id": turnID})

	var output string
	interrupted := false
	var interruptDeadline <-chan time.Time
	done := tctx.Done()
	for {
		select {
		case <-done:
			done = nil
			interrupted = true
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = rpc.call(ictx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
			cancel()
			emit(harnesses.EventProgress, "", map[string]any{"phase": "interrupt_sent", "turn_id": turnID})
			interruptDeadline = time.After(a.grace)
		case <-interruptDeadline:
			// The turn did not wind down: kill the app-server; the next turn
			// restarts it and resumes the thread.
			a.mu.Lock()
			a.killLocked()
			a.mu.Unlock()
			res.Status, res.Error = harnesses.TurnInterrupted, "interrupt not acknowledged; app-server restarted"
			return finish()
		case <-rpc.closed:
			a.mu.Lock()
			stopping := a.stopping
			a.mu.Unlock()
			res.Status, res.Error = harnesses.TurnFailed, "codex app-server exited"+a.stderrNote()
			if interrupted || stopping {
				res.Status, res.Error = harnesses.TurnInterrupted, "stopped"
			}
			return finish()
		case m := <-notes:
			switch m.Method {
			case "turn/completed":
				var p struct {
					Turn turnInfo `json:"turn"`
				}
				_ = json.Unmarshal(m.Params, &p)
				if p.Turn.ID != turnID {
					continue
				}
				res.Output = output
				switch p.Turn.Status {
				case "completed":
					res.Status = harnesses.TurnCompleted
				case "interrupted":
					res.Status, res.Error = harnesses.TurnInterrupted, "interrupted"
				default:
					res.Status = harnesses.TurnFailed
					res.Error = "turn " + p.Turn.Status
					if p.Turn.Error != nil {
						res.Error = p.Turn.Error.Message
					}
				}
				a.mu.Lock()
				if res.Status == harnesses.TurnCompleted {
					a.session.Turns++
				}
				if recovery != nil {
					res.Recovery = &harnesses.ResumeResult{Mode: recovery.Mode, Note: recovery.Note, SessionID: threadID}
					a.recovery = nil
				}
				if err := a.saveSessionLocked(); err != nil && res.Status == harnesses.TurnCompleted {
					res.Status, res.Error = harnesses.TurnFailed, err.Error()
				}
				a.mu.Unlock()
				return finish()
			case "item/started", "item/completed":
				if text := a.itemEvent(m, emit); text != "" {
					output = text
				}
			case "error":
				var p struct {
					Error     json.RawMessage `json:"error"`
					WillRetry bool            `json:"willRetry"`
				}
				_ = json.Unmarshal(m.Params, &p)
				emit(harnesses.EventError, "", map[string]any{"error": json.RawMessage(p.Error), "will_retry": p.WillRetry})
			case "warning":
				emit(harnesses.EventProgress, "", map[string]any{"phase": "warning", "detail": json.RawMessage(m.Params)})
			}
		}
	}
}

// item is the subset of a ThreadItem the adapter reads (CONTRACT.md §1).
type item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	Text    string          `json:"text"`
	Command json.RawMessage `json:"command"`
}

// itemEvent emits events for a started or completed item and returns the
// text of a completed agent message.
func (a *Adapter) itemEvent(m rpcMessage, emit func(kind, corr string, data any)) string {
	var p struct {
		Item item `json:"item"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return ""
	}
	it := p.Item
	completed := m.Method == "item/completed"
	switch it.Type {
	case "mcpToolCall":
		name := "mcp__" + it.Server + "__" + it.Tool
		if !completed {
			emit(harnesses.EventToolRequest, it.ID, map[string]any{"name": name, "input": json.RawMessage(nonEmpty(it.Arguments))})
			return ""
		}
		data := map[string]any{"name": name, "is_error": it.Status == "failed" || it.Error != nil, "content": json.RawMessage(nonEmpty(it.Result))}
		if it.Error != nil {
			data["error"] = it.Error.Message
		}
		emit(harnesses.EventToolResult, it.ID, data)
	case "commandExecution", "fileChange", "webSearch", "dynamicToolCall":
		if !completed {
			emit(harnesses.EventToolRequest, it.ID, map[string]any{"name": it.Type, "input": json.RawMessage(nonEmpty(it.Command))})
		} else {
			emit(harnesses.EventToolResult, it.ID, map[string]any{"name": it.Type, "is_error": it.Status == "failed"})
		}
	case "agentMessage":
		if completed {
			emit(harnesses.EventOutput, it.ID, map[string]any{"text": harnesses.TruncateUTF8(it.Text, 8192)})
			return it.Text
		}
	}
	return ""
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
