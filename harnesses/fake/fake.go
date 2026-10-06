// Package fake is a deterministic harness adapter for infrastructure tests.
//
// It interprets each delivered message body as a tiny script. Every line that
// starts with "/" is a command; other lines are ignored when commands are
// present. Platform tools are always invoked through the steadmesh-tools CLI
// binary (Environment.ToolCommand), the same path and authority a real
// harness uses.
//
//	/tool <name> [json]      call a platform tool
//	/reply <text>            messages.reply to the triggering message
//	/file write <path> <text> write a workspace file (relative to /seat/workspace)
//	/file read <path>        read a workspace file into the turn output
//	/sleep <duration>        sleep (interruptible; max 10m)
//	/delegate <seat> <text>  messages.send to <seat> with correlation_id = the
//	                         triggering message; all following lines are part of
//	                         <text>. The human message is recorded in
//	                         <runner>/fake-state.json so the eventual reply can
//	                         be forwarded.
//	/report                  messages.reply with the outputs collected so far
//	/claim <text>            the agent claims business completion (informational)
//	/fail <text>             fail the turn technically (for tests)
//
// A body with no commands gets "ack: <first 200 chars>" via messages.reply
// (unless it is itself a reply from a seat, to avoid ack loops). A seat
// message whose body starts with "/task" has its remaining lines executed
// and the outputs replied to the sender. A seat reply correlated with a
// recorded delegation is forwarded to the original human message.
//
// Session continuity: <home>/.fake-session counts turns and records recent
// message ids, so restart continuity is observable.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

const (
	// AdapterName is the harness profile adapter value.
	AdapterName = "fake"
	// FormatVersion is the session format identifier.
	FormatVersion = "fake-session/1"
	// SessionFileName lives in HOME; StateFileName in the runner dir.
	SessionFileName = ".fake-session"
	StateFileName   = "fake-state.json"

	maxSleep      = 10 * time.Minute
	maxRecentIDs  = 20
	maxOutputLine = 4096
)

// Session is the content of <home>/.fake-session.
type Session struct {
	SessionID      string    `json:"session_id"`
	Turns          int       `json:"turns"`
	LastMessageIDs []string  `json:"last_message_ids"`
	ConfigRevision string    `json:"config_revision,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Delegation records a human message whose work was delegated.
type Delegation struct {
	HumanMessageID string `json:"human_message_id"`
	Binding        string `json:"binding,omitempty"`
	DelegatedTo    string `json:"delegated_to"`
	SentMessageID  string `json:"sent_message_id,omitempty"`
}

// State is the content of <runner>/fake-state.json.
type State struct {
	// Delegations is keyed by both the human message id and the id of the
	// delegation message sent, so either correlation finds it.
	Delegations map[string]Delegation `json:"delegations"`
}

// Adapter is the fake harness.
type Adapter struct {
	mu      sync.Mutex
	env     harnesses.Environment
	bus     *harnesses.EventBus
	guard   harnesses.TurnGuard
	started bool
	session Session
	note    string
}

// New returns a fake adapter.
func New() *Adapter { return &Adapter{bus: harnesses.NewEventBus(1024)} }

var _ harnesses.Adapter = (*Adapter)(nil)

func (a *Adapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{
		Adapter:              AdapterName,
		Version:              FormatVersion,
		ToolTransports:       []string{"cli"},
		EventStreaming:       true,
		Interruption:         "cooperative",
		RecoveryModes:        []string{harnesses.RecoveryFresh, harnesses.RecoveryNativeResume, harnesses.RecoveryPortableHandoff},
		CheckpointGuarantees: []string{harnesses.GuaranteeApplication},
		GracefulSuspension:   false,
		SessionFormat:        FormatVersion,
	}
}

func (a *Adapter) Prepare(ctx context.Context, env harnesses.Environment) error {
	if env.ToolCommand == "" {
		return errors.New("fake: ToolCommand is required")
	}
	if _, err := os.Stat(env.ToolCommand); err != nil {
		if _, lerr := exec.LookPath(env.ToolCommand); lerr != nil {
			return fmt.Errorf("fake: tool command: %w", err)
		}
	}
	for _, d := range []string{env.WorkspaceDir, env.HomeDir, env.RunnerDir} {
		if d == "" {
			return errors.New("fake: workspace, home and runner dirs are required")
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("fake: %w", err)
		}
	}
	a.mu.Lock()
	a.env = env
	a.mu.Unlock()
	return nil
}

func (a *Adapter) sessionPath() string { return filepath.Join(a.env.HomeDir, SessionFileName) }
func (a *Adapter) statePath() string   { return filepath.Join(a.env.RunnerDir, StateFileName) }

func (a *Adapter) StartOrResume(ctx context.Context, rec harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.env.HomeDir == "" {
		return harnesses.ResumeResult{}, errors.New("fake: Prepare has not been called")
	}
	var s Session
	b, err := os.ReadFile(a.sessionPath())
	switch {
	case err == nil && json.Unmarshal(b, &s) == nil && s.SessionID != "":
		a.session = s
		a.started = true
		return harnesses.ResumeResult{Mode: harnesses.RecoveryNativeResume, SessionID: s.SessionID}, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return harnesses.ResumeResult{}, fmt.Errorf("fake: read session: %w", err)
	}
	a.session = Session{SessionID: "fake-" + uuid.NewString(), ConfigRevision: a.env.ConfigRevision}
	a.started = true
	res := harnesses.ResumeResult{Mode: harnesses.RecoveryFresh, SessionID: a.session.SessionID}
	if rec.Session != nil {
		res.Mode = harnesses.RecoveryPortableHandoff
		if rec.Session.HarnessAdapter != AdapterName {
			res.Note = fmt.Sprintf("native session from adapter %q (format %s) is not convertible; started a new fake session from the portable handoff", rec.Session.HarnessAdapter, rec.Session.FormatVersion)
		} else {
			res.Note = fmt.Sprintf("native fake session %s was not found on the workspace volume; started a new session from the portable handoff", rec.Session.CheckpointRef)
		}
		a.note = res.Note
	}
	if err := a.saveSessionLocked(); err != nil {
		return harnesses.ResumeResult{}, err
	}
	return res, nil
}

func (a *Adapter) saveSessionLocked() error {
	a.session.UpdatedAt = time.Now().UTC()
	b, _ := json.MarshalIndent(a.session, "", "  ")
	tmp := a.sessionPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("fake: write session: %w", err)
	}
	return os.Rename(tmp, a.sessionPath())
}

// SessionSnapshot returns a copy of the current session state.
func (a *Adapter) SessionSnapshot() Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.session
	s.LastMessageIDs = append([]string(nil), s.LastMessageIDs...)
	return s
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
	if err := a.saveSessionLocked(); err != nil {
		return runtimeapi.Checkpoint{}, err
	}
	return runtimeapi.Checkpoint{
		HarnessAdapter: AdapterName,
		FormatVersion:  FormatVersion,
		CheckpointRef:  fmt.Sprintf("%s:turns=%d", a.session.SessionID, a.session.Turns),
		Guarantee:      harnesses.GuaranteeApplication,
	}, nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	done := a.guard.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		// The fake runs in-process: the cancelled turn context aborts tool
		// subprocesses (exec.CommandContext kills them), so wait briefly.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	a.bus.Close()
	return nil
}

// ---- turn execution ---------------------------------------------------------

type turn struct {
	a       *Adapter
	ctx     context.Context
	d       harnesses.Delivery
	outputs []string
	replied bool
	claim   *harnesses.Claim
	failErr error
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
	t := &turn{a: a, ctx: tctx, d: d}
	t.emit(harnesses.EventProgress, "", map[string]any{"phase": "turn_started", "message_id": d.Message.MessageID, "origin": d.Message.Origin})
	a.mu.Lock()
	note := a.note
	a.note = ""
	a.mu.Unlock()
	if note != "" {
		t.emit(harnesses.EventProgress, "", map[string]any{"phase": "recovery", "note": note})
	}

	t.run()

	res := harnesses.TurnResult{MessageID: d.Message.MessageID, Status: harnesses.TurnCompleted, Output: strings.Join(t.outputs, "\n"), Duration: time.Since(start), Claim: t.claim}
	if st, msg := harnesses.StatusFor(tctx); st != "" {
		res.Status, res.Error = st, msg
	} else if t.failErr != nil {
		res.Status, res.Error = harnesses.TurnFailed, t.failErr.Error()
	}
	a.mu.Lock()
	res.SessionID = a.session.SessionID
	if res.Status == harnesses.TurnCompleted {
		a.session.Turns++
		a.session.LastMessageIDs = append(a.session.LastMessageIDs, d.Message.MessageID)
		if n := len(a.session.LastMessageIDs); n > maxRecentIDs {
			a.session.LastMessageIDs = a.session.LastMessageIDs[n-maxRecentIDs:]
		}
		a.session.ConfigRevision = a.env.ConfigRevision
		if err := a.saveSessionLocked(); err != nil {
			res.Status, res.Error = harnesses.TurnFailed, err.Error()
		}
	}
	a.mu.Unlock()
	if note != "" {
		res.Recovery = &harnesses.ResumeResult{Mode: harnesses.RecoveryPortableHandoff, Note: note}
	}
	if res.Claim != nil {
		t.emit(harnesses.EventOutput, "", map[string]any{"claim": res.Claim})
	}
	t.emit(harnesses.EventCompletion, "", map[string]any{"status": res.Status, "error": res.Error, "session_id": res.SessionID, "duration_ms": res.Duration.Milliseconds()})
	return res, nil
}

func (t *turn) emit(kind, corr string, data any) {
	t.a.bus.Emit(t.ctx, harnesses.Event{
		Kind: kind, ExecutionID: t.d.ExecutionID, MessageID: t.d.Message.MessageID,
		CorrelationID: corr, Data: harnesses.MustJSON(data),
	})
}

func (t *turn) out(s string) {
	s = harnesses.TruncateUTF8(s, maxOutputLine)
	t.outputs = append(t.outputs, s)
	t.emit(harnesses.EventOutput, "", map[string]string{"text": s})
}

func (t *turn) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	t.outputs = append(t.outputs, "error: "+msg)
	t.emit(harnesses.EventError, "", map[string]string{"error": msg})
}

func (t *turn) run() {
	m := t.d.Message
	body := strings.ReplaceAll(m.Body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	// A delegated task from another seat.
	if m.Origin == "seat" && strings.HasPrefix(strings.TrimSpace(body), "/task") {
		first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "/task"))
		rest := lines[1:]
		if first != "" {
			rest = append([]string{first}, rest...)
		}
		t.exec(rest)
		if t.ctx.Err() != nil {
			return
		}
		summary := strings.Join(t.outputs, "\n")
		if summary == "" {
			summary = "task done (no output)"
		}
		t.reply(map[string]any{"message_id": m.MessageID, "body": harnesses.TruncateUTF8("task result from "+t.a.env.SeatKey+":\n"+summary, 8000)})
		return
	}

	// A reply from a seat to an earlier delegation: forward to the human.
	if m.Origin == "seat" {
		if del, key, ok := t.a.lookupDelegation(m.CorrelationID, m.ParentID); ok {
			args := map[string]any{"body": harnesses.TruncateUTF8(fmt.Sprintf("Update from %s: %s", m.SenderSeat, m.Body), 8000)}
			if del.HumanMessageID != "" {
				args["message_id"] = del.HumanMessageID
			} else {
				args["binding"] = del.Binding
			}
			if t.reply(args) {
				t.a.removeDelegation(key, del)
			}
			return
		}
	}

	if !hasCommand(lines) {
		if m.Origin == "seat" && (m.ParentID != "" || strings.HasPrefix(body, "ack: ")) {
			t.out("received reply; no response needed")
			return
		}
		t.reply(map[string]any{"message_id": m.MessageID, "body": "ack: " + harnesses.TruncateUTF8(body, 200)})
		return
	}
	t.exec(lines)
}

func hasCommand(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "/") {
			return true
		}
	}
	return false
}

func (t *turn) exec(lines []string) {
	for i := 0; i < len(lines); i++ {
		if t.ctx.Err() != nil || t.failErr != nil {
			return
		}
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "/") {
			continue
		}
		cmd, rest := splitWord(line[1:])
		switch cmd {
		case "tool":
			name, args := splitWord(rest)
			if name == "" {
				t.errorf("/tool requires a name")
				continue
			}
			if args == "" {
				args = "{}"
			}
			if res, isErr, err := t.tool(name, json.RawMessage(args)); err == nil {
				prefix := "tool " + name + ": "
				if isErr {
					prefix = "tool " + name + " error: "
				}
				t.out(prefix + res)
			}
		case "reply":
			t.reply(map[string]any{"message_id": t.d.Message.MessageID, "body": rest})
		case "file":
			t.file(rest)
		case "sleep":
			dur, err := time.ParseDuration(strings.TrimSpace(rest))
			if err != nil || dur < 0 {
				t.errorf("/sleep: invalid duration %q", rest)
				continue
			}
			dur = min(dur, maxSleep)
			select {
			case <-time.After(dur):
				t.out("slept " + dur.String())
			case <-t.ctx.Done():
				return
			}
		case "delegate":
			seat, text := splitWord(rest)
			if seat == "" {
				t.errorf("/delegate requires a seat")
				continue
			}
			if i+1 < len(lines) {
				text = strings.TrimSpace(text + "\n" + strings.Join(lines[i+1:], "\n"))
			}
			t.delegate(seat, text)
			return
		case "report":
			summary := strings.Join(t.outputs, "\n")
			if summary == "" {
				summary = "(no output)"
			}
			t.reply(map[string]any{"message_id": t.d.Message.MessageID, "body": harnesses.TruncateUTF8(summary, 8000)})
		case "claim":
			t.claim = &harnesses.Claim{Kind: "task_done", Text: rest}
			t.out("claimed: " + rest)
		case "fail":
			t.failErr = errors.New("scripted failure: " + rest)
			t.emit(harnesses.EventError, "", map[string]string{"error": t.failErr.Error()})
		default:
			t.errorf("unknown command /%s", cmd)
		}
	}
}

func splitWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func (t *turn) reply(args map[string]any) bool {
	b, _ := json.Marshal(args)
	res, isErr, err := t.tool("messages.reply", b)
	if err != nil {
		return false
	}
	if isErr {
		t.errorf("messages.reply rejected: %s", res)
		return false
	}
	t.replied = true
	t.out("replied: " + res)
	return true
}

func (t *turn) delegate(seat, text string) {
	m := t.d.Message
	args, _ := json.Marshal(map[string]any{"to": seat, "body": text, "correlation_id": m.MessageID})
	res, isErr, err := t.tool("messages.send", args)
	if err != nil {
		return
	}
	if isErr {
		t.errorf("messages.send rejected: %s", res)
		return
	}
	t.out("delegated to " + seat + ": " + res)
	human := m.MessageID
	binding := m.Binding
	// Delegating on behalf of a delegation keeps the original human.
	if m.Origin != "human" {
		if del, _, ok := t.a.lookupDelegation(m.CorrelationID, m.ParentID); ok {
			human, binding = del.HumanMessageID, del.Binding
		}
	}
	del := Delegation{HumanMessageID: human, Binding: binding, DelegatedTo: seat, SentMessageID: messageIDFrom(res)}
	if err := t.a.recordDelegation(m.MessageID, del); err != nil {
		t.errorf("record delegation: %v", err)
	}
}

// messageIDFrom extracts a message id from a messages.send result.
func messageIDFrom(res string) string {
	var v map[string]any
	if json.Unmarshal([]byte(res), &v) != nil {
		return ""
	}
	for _, k := range []string{"message_id", "id"} {
		if s, ok := v[k].(string); ok {
			return s
		}
	}
	if msg, ok := v["message"].(map[string]any); ok {
		if s, ok := msg["message_id"].(string); ok {
			return s
		}
	}
	return ""
}

func (t *turn) file(rest string) {
	op, rest := splitWord(rest)
	path, content := splitWord(rest)
	full, err := t.a.workspacePath(path)
	if err != nil {
		t.errorf("/file: %v", err)
		return
	}
	switch op {
	case "write":
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.errorf("/file write: %v", err)
			return
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.errorf("/file write: %v", err)
			return
		}
		t.out(fmt.Sprintf("wrote %s (%d bytes)", path, len(content)))
	case "read":
		b, err := os.ReadFile(full)
		if err != nil {
			t.errorf("/file read: %v", err)
			return
		}
		t.out("file " + path + ": " + string(b))
	default:
		t.errorf("/file: unknown operation %q", op)
	}
}

func (a *Adapter) workspacePath(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is required")
	}
	ws := filepath.Clean(a.env.WorkspaceDir)
	full := filepath.Join(ws, filepath.Clean("/"+rel))
	r, err := filepath.Rel(ws, full)
	if err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	return full, nil
}

// tool runs steadmesh-tools call <name> - with args on stdin. err is non-nil
// for transport failures (which also fail the turn when fenced).
func (t *turn) tool(name string, args json.RawMessage) (string, bool, error) {
	corr := uuid.NewString()
	t.emit(harnesses.EventToolRequest, corr, map[string]any{"name": name, "arguments": json.RawMessage(args)})
	cmd := exec.CommandContext(t.ctx, t.a.env.ToolCommand, "call", name, "-")
	cmd.Stdin = bytes.NewReader(args)
	cmd.Dir = t.a.env.WorkspaceDir
	cmd.Env = append(os.Environ(), t.a.env.ExtraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := strings.TrimSpace(stdout.String())
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	switch code {
	case 0:
		t.emit(harnesses.EventToolResult, corr, map[string]any{"name": name, "content": rawOrString(res)})
		return res, false, nil
	case 1:
		t.emit(harnesses.EventToolResult, corr, map[string]any{"name": name, "is_error": true, "content": rawOrString(res)})
		return res, true, nil
	default:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		e := fmt.Errorf("tool %s failed (exit %d): %s", name, code, msg)
		t.emit(harnesses.EventToolResult, corr, map[string]any{"name": name, "is_error": true, "transport_error": msg})
		t.errorf("%v", e)
		if code == 4 { // fenced: this execution must stop writing
			t.failErr = e
		}
		return "", false, e
	}
}

func rawOrString(s string) any {
	if json.Valid([]byte(s)) && s != "" {
		return json.RawMessage(s)
	}
	return s
}

// ---- delegation state -------------------------------------------------------

func (a *Adapter) loadState() State {
	var s State
	b, err := os.ReadFile(a.statePath())
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Delegations == nil {
		s.Delegations = map[string]Delegation{}
	}
	return s
}

func (a *Adapter) saveState(s State) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := a.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath())
}

func (a *Adapter) recordDelegation(triggerID string, d Delegation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.loadState()
	s.Delegations[triggerID] = d
	if d.SentMessageID != "" {
		s.Delegations[d.SentMessageID] = d
	}
	return a.saveState(s)
}

func (a *Adapter) lookupDelegation(ids ...string) (Delegation, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.loadState()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if d, ok := s.Delegations[id]; ok {
			return d, id, true
		}
	}
	return Delegation{}, "", false
}

func (a *Adapter) removeDelegation(key string, d Delegation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.loadState()
	delete(s.Delegations, key)
	for k, v := range s.Delegations {
		if v == d {
			delete(s.Delegations, k)
		}
	}
	_ = a.saveState(s)
}
