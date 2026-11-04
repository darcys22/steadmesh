//go:build linux

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

const RestrictedAdapterName = "acp-codex-restricted-experimental"
const portableFormat = "acp-portable-handoff-v1"

// turnLauncher owns one fresh process/session and must tear it down before Run
// returns, including on cancellation. Callbacks are synchronous observations.
type turnLauncher interface {
	Run(context.Context, harnesses.Environment, string, func(Observation)) (Observation, error)
}

// RestrictedAdapter is an unregistered, Linux-only lifecycle experiment. The
// host must meet ExperimentalLauncher's isolation requirements. It deliberately
// does not implement native resume, managed skills, extra MCP or workspace cwd.
// Construct with NewRestricted; the zero value is not usable.
type RestrictedAdapter struct {
	mu                sync.Mutex
	guard             harnesses.TurnGuard
	bus               *harnesses.EventBus
	launcher          turnLauncher
	config            ExperimentalLauncher
	env               harnesses.Environment
	prepared, started bool
	recovery          harnesses.ResumeResult
	stop              sync.Once
}

// NewRestricted explicitly selects the restricted launcher, never a registry
// entry or profile override. Prepare validates configuration without execution;
// filesystem integrity and executable pins are checked by Run on every turn.
func NewRestricted(config ExperimentalLauncher) *RestrictedAdapter {
	return &RestrictedAdapter{config: config, launcher: config, bus: harnesses.NewEventBus(256)}
}

var _ harnesses.Adapter = (*RestrictedAdapter)(nil)

func (*RestrictedAdapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{Adapter: RestrictedAdapterName, Version: CodexACPVersion,
		ToolTransports: []string{"mcp"}, EventStreaming: true, Interruption: "signal",
		RecoveryModes:        []string{harnesses.RecoveryFresh, harnesses.RecoveryPortableHandoff},
		CheckpointGuarantees: []string{harnesses.GuaranteeApplication}, StructuredOutputMode: "platform_validated"}
}

func (a *RestrictedAdapter) Prepare(ctx context.Context, env harnesses.Environment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.guard.Stopped() {
		return harnesses.ErrStopped
	}
	if a.prepared {
		return errors.New("acp: already prepared")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(env.HarnessConfig) != 0 {
		return errors.New("acp: restricted adapter accepts no harness config")
	}
	if len(env.MCPServers) != 0 {
		return errors.New("acp: restricted adapter supports only the platform bridge")
	}
	for _, value := range env.ExtraEnv {
		key, _, _ := strings.Cut(value, "=")
		if key == "STEADMESH_EXECUTION" || key == "STEADMESH_EXECUTION_FILE" {
			return errors.New("acp: restricted adapter requires runner-owned execution attribution")
		}
	}
	for _, p := range []string{a.config.NodePath, a.config.ACPPath, a.config.CodexPath, a.config.NeutralDir, a.config.PrivateParent} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("acp: canonical absolute launcher paths required")
		}
	}
	if _, err := PlanCodex(ctx, env, a.config.CodexPath); err != nil {
		return err
	}
	// Own mutable input data: caller changes must not switch seat/model authority
	// after Prepare. The loader remains live for authorization at each boundary.
	env.ExtraEnv = append([]string(nil), env.ExtraEnv...)
	env.SandboxEnv = append([]string(nil), env.SandboxEnv...)
	m := *env.Model
	m.Settings = make(map[string]string, len(env.Model.Settings))
	for k, v := range env.Model.Settings {
		m.Settings[k] = v
	}
	env.Model = &m
	raw, err := json.Marshal(env.Bootstrap)
	if err != nil {
		return err
	}
	var bootstrap runtimeapi.Bootstrap
	if err := json.Unmarshal(raw, &bootstrap); err != nil {
		return err
	}
	env.Bootstrap = bootstrap
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.guard.Stopped() {
		return harnesses.ErrStopped
	}
	a.env, a.prepared = env, true
	return nil
}

func (a *RestrictedAdapter) StartOrResume(ctx context.Context, rec harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.guard.Stopped() {
		return harnesses.ResumeResult{}, harnesses.ErrStopped
	}
	if err := ctx.Err(); err != nil {
		return harnesses.ResumeResult{}, err
	}
	if !a.prepared {
		return harnesses.ResumeResult{}, errors.New("acp: Prepare has not been called")
	}
	if a.started {
		return harnesses.ResumeResult{}, errors.New("acp: already started")
	}
	r := harnesses.ResumeResult{Mode: harnesses.RecoveryFresh, Note: "Each delivery starts a fresh restricted session in a neutral directory; native session state is not retained. Use platform handoff and memory tools to recover durable context."}
	if rec.Session != nil || rec.Handoff != nil || rec.Note != "" || rec.PendingMessages != 0 || len(rec.UnknownOperations) != 0 {
		r.Mode = harnesses.RecoveryPortableHandoff
	}
	// The explicit descriptor takes precedence over stale bootstrap recovery.
	b := &a.env.Bootstrap.Recovery
	b.Session, b.Handoff, b.Note = nil, rec.Handoff, rec.Note
	b.PendingMessages, b.UnknownOperations = rec.PendingMessages, rec.UnknownOperations
	raw, err := json.Marshal(a.env.Bootstrap)
	if err != nil {
		return harnesses.ResumeResult{}, err
	}
	var bootstrap runtimeapi.Bootstrap
	if err := json.Unmarshal(raw, &bootstrap); err != nil {
		return harnesses.ResumeResult{}, err
	}
	a.env.Bootstrap = bootstrap
	a.recovery, a.started = r, true
	return r, nil
}

func (a *RestrictedAdapter) Events() <-chan harnesses.Event { return a.bus.C() }

func (a *RestrictedAdapter) Deliver(ctx context.Context, d harnesses.Delivery) (harnesses.TurnResult, error) {
	a.mu.Lock()
	started, env, recovery := a.started, a.env, a.recovery
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
	res := harnesses.TurnResult{MessageID: d.Message.MessageID, Status: harnesses.TurnFailed, Recovery: &recovery}
	emit := func(kind, corr string, data any) {
		a.bus.Emit(tctx, harnesses.Event{Kind: kind, ExecutionID: d.ExecutionID, MessageID: d.Message.MessageID, CorrelationID: corr, Data: harnesses.MustJSON(data)})
	}
	emit(harnesses.EventProgress, "", map[string]string{"phase": "turn_started", "mode": "fresh_restricted_session"})
	var output strings.Builder
	if err = CheckManagedSkills(tctx, env); err == nil {
		err = tctx.Err()
	}
	if err == nil {
		var terminal Observation
		terminal, err = a.launcher.Run(tctx, env, recovery.Note+"\n\n"+harnesses.RenderEnvelope(d), func(o Observation) {
			switch o.Kind {
			case "output":
				output.WriteString(o.Text)
				emit(harnesses.EventOutput, "", map[string]string{"text": o.Text})
			case "tool_call", "tool_call_update":
				// ACP tool updates do not establish success, execution or permissions.
				emit(harnesses.EventProgress, o.ToolCallID, map[string]string{"phase": o.Kind})
			}
		})
		if err == nil && terminal.Kind != "stop" {
			err = errors.New("acp: missing terminal observation")
		}
		if err == nil {
			switch terminal.StopReason {
			case "end_turn":
				res.Status = harnesses.TurnCompleted
			case "cancelled":
				res.Status, res.Error = harnesses.TurnInterrupted, "ACP prompt cancelled"
			case "max_tokens", "max_turn_requests", "refusal":
				res.Error = "ACP prompt stopped: " + terminal.StopReason
			default:
				res.Error = "ACP prompt missing valid terminal result"
			}
		}
	}
	if err != nil {
		res.Error = "restricted ACP turn failed"
	} // Do not leak arbitrary launcher diagnostics.
	if status, reason := harnesses.StatusFor(tctx); status != "" {
		res.Status, res.Error = status, reason
	}
	res.Output, res.Duration = output.String(), time.Since(start)
	emit(harnesses.EventCompletion, "", map[string]any{"status": res.Status, "error": res.Error, "duration_ms": res.Duration.Milliseconds()})
	return res, nil
}

func (a *RestrictedAdapter) Quiesce(ctx context.Context) (harnesses.QuiesceResult, error) {
	return a.guard.Quiesce(ctx), nil
}

// Checkpoint persists only a descriptor. The portable handoff itself is owned
// and persisted by the platform's handoff tools, not a native transcript here.
func (a *RestrictedAdapter) Checkpoint(ctx context.Context) (runtimeapi.Checkpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return runtimeapi.Checkpoint{}, harnesses.ErrNotStarted
	}
	if err := ctx.Err(); err != nil {
		return runtimeapi.Checkpoint{}, err
	}
	cp := runtimeapi.Checkpoint{HarnessAdapter: RestrictedAdapterName, FormatVersion: portableFormat, CheckpointRef: "none", Guarantee: harnesses.GuaranteeApplication}
	data, err := json.Marshal(cp)
	if err != nil {
		return runtimeapi.Checkpoint{}, err
	}
	f, err := os.CreateTemp(a.env.RunnerDir, ".acp-checkpoint-")
	if err != nil {
		return runtimeapi.Checkpoint{}, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(a.env.RunnerDir, "acp-portable-checkpoint.json"))
	}
	if err != nil {
		return runtimeapi.Checkpoint{}, err
	}
	return cp, nil
}

func (a *RestrictedAdapter) Stop(ctx context.Context) error {
	done := a.guard.Stop()
	// Close only after Deliver's final event and process cleanup. Even when the
	// caller's deadline expires, the owned cancellation continues to completion.
	a.stop.Do(func() { go func() { <-done; a.bus.Close() }() })
	select {
	case <-done:
		a.bus.Close()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
