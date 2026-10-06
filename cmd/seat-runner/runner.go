package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// Exit codes.
const (
	exitOK           = 0
	exitError        = 1
	exitLeaseTimeout = 2
	exitFenced       = 3
)

// Errors returned by Run.
var (
	ErrFenced       = errors.New("seat lease lost (fenced); harness killed")
	ErrLeaseTimeout = errors.New("could not acquire the seat lease in time")
)

// ExitCode maps a Run error to a process exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, ErrFenced):
		return exitFenced
	case errors.Is(err, ErrLeaseTimeout):
		return exitLeaseTimeout
	default:
		return exitError
	}
}

// Seat states reported to the platform.
const (
	StateWarm      = "Warm"
	StateExecuting = "Executing"
	StateQuiescing = "Quiescing"
	StateStopped   = "Stopped"
)

// AdapterFactory creates a harness adapter by name.
type AdapterFactory func(name string) (harnesses.Adapter, error)

// Runner is the in-Pod seat supervisor.
type Runner struct {
	cfg        Config
	c          *client.Client
	log        *slog.Logger
	newAdapter AdapterFactory

	gen        atomic.Int64
	leaseHeld  atomic.Bool
	bootLoaded atomic.Bool

	workCtx    context.Context
	cancelWork context.CancelCauseFunc
	fenceOnce  sync.Once
	fencedFlag atomic.Bool

	mu      sync.Mutex
	adapter harnesses.Adapter
	boot    runtimeapi.Bootstrap
	fwd     *forwarder
	// modelConn is the resolved model connection (env or manifest).
	modelConn string
}

// NewRunner builds a runner.
func NewRunner(cfg Config, log *slog.Logger, f AdapterFactory) (*Runner, error) {
	r := &Runner{cfg: cfg, log: log, newAdapter: f}
	c, err := client.New(client.Config{
		BaseURL:    cfg.PlatformURL,
		TokenFile:  cfg.TokenFile,
		Generation: r.gen.Load,
		Execution:  client.ExecutionFromEnvOrFile(),
		UserAgent:  "seat-runner",
	})
	if err != nil {
		return nil, err
	}
	r.c = c
	return r, nil
}

// Ready reports readiness: the lease is held and the bootstrap has loaded.
func (r *Runner) Ready() bool { return r.leaseHeld.Load() && r.bootLoaded.Load() }

// Handler serves /healthz and /readyz.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(runtimeapi.PathHealthz, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc(runtimeapi.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			http.Error(w, fmt.Sprintf("not ready: lease_held=%v bootstrap_loaded=%v", r.leaseHeld.Load(), r.bootLoaded.Load()), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

// fence handles lease loss: kill the harness immediately so the stale
// writer stops (§6.3), then abort all work.
func (r *Runner) fence(cause error) {
	r.fenceOnce.Do(func() {
		r.fencedFlag.Store(true)
		r.leaseHeld.Store(false)
		r.log.Error("seat lease lost; killing harness and exiting", "generation", r.gen.Load(), "cause", cause)
		r.mu.Lock()
		a := r.adapter
		r.mu.Unlock()
		if a != nil {
			expired, cancel := context.WithCancel(context.Background())
			cancel()
			_ = a.Stop(expired)
		}
		r.cancelWork(fmt.Errorf("%w: %v", ErrFenced, cause))
	})
}

// check routes fenced API errors to fence and returns err unchanged.
func (r *Runner) check(err error) error {
	if err != nil && client.IsFenced(err) {
		r.fence(err)
	}
	return err
}

func (r *Runner) reportState(ctx context.Context, state, detail string) {
	req := runtimeapi.StateRequest{Generation: r.gen.Load(), State: state, Detail: harnesses.TruncateUTF8(detail, 1024), AdoptedRevision: r.cfg.ConfigRevision}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.check(r.c.ReportState(cctx, req)); err != nil {
		r.log.Warn("report state failed", "state", state, "err", err)
		return
	}
	r.log.Info("state", "state", state, "detail", req.Detail, "adopted_revision", req.AdoptedRevision)
}

// Run supervises the seat until stop is cancelled (SIGTERM) or the lease is lost.
func (r *Runner) Run(stop context.Context) error {
	r.workCtx, r.cancelWork = context.WithCancelCause(context.Background())
	defer r.cancelWork(nil)

	if err := r.acquire(stop); err != nil {
		return err
	}
	if stop.Err() != nil {
		r.release()
		return nil
	}
	renewDone := make(chan struct{})
	renewStop := make(chan struct{})
	go func() { defer close(renewDone); r.renewLoop(renewStop) }()
	defer func() { close(renewStop); <-renewDone }()

	if err := r.start(stop); err != nil {
		if r.fencedFlag.Load() {
			return context.Cause(r.workCtx)
		}
		r.log.Error("seat start failed", "err", err)
		r.reportState(r.workCtx, StateStopped, "start failed: "+err.Error())
		r.shutdownAdapter()
		r.release()
		return err
	}
	r.reportState(r.workCtx, StateWarm, "")

	// Quiesce on SIGTERM, concurrently with an in-flight turn.
	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		select {
		case <-stop.Done():
		case <-r.workCtx.Done():
			return
		}
		r.reportState(r.workCtx, StateQuiescing, "termination requested")
		qctx, cancel := context.WithTimeout(r.workCtx, r.cfg.QuiesceTimeout)
		defer cancel()
		res, err := r.adapter.Quiesce(qctx)
		r.log.Info("quiesced", "interrupted", res.Interrupted, "safe_point", res.SafePoint, "err", err)
	}()

	loopErr := r.loop(stop)
	if r.fencedFlag.Load() {
		return context.Cause(r.workCtx)
	}
	if loopErr == nil {
		<-quiesced
	}
	if r.fencedFlag.Load() {
		return context.Cause(r.workCtx)
	}
	r.checkpoint(r.workCtx)
	r.shutdownAdapter()
	if r.fencedFlag.Load() {
		return context.Cause(r.workCtx)
	}
	detail := "graceful shutdown"
	if loopErr != nil {
		detail = "runner error: " + loopErr.Error()
	}
	r.reportState(r.workCtx, StateStopped, detail)
	r.release()
	return loopErr
}

func (r *Runner) shutdownAdapter() {
	r.mu.Lock()
	a, fwd := r.adapter, r.fwd
	r.mu.Unlock()
	if a == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = a.Stop(sctx)
	if fwd != nil {
		fwd.Wait(sctx)
	}
}

// acquire obtains the lease, retrying while it is held elsewhere.
func (r *Runner) acquire(stop context.Context) error {
	deadline := time.Now().Add(r.cfg.LeaseAcquireTimeout)
	attempt := 0
	holder := r.cfg.PodUID
	if holder == "" {
		holder, _ = os.Hostname()
	}
	for {
		attempt++
		ctx, cancel := context.WithTimeout(stop, 15*time.Second)
		lease, err := r.c.AcquireLease(ctx, holder)
		cancel()
		if err == nil {
			r.gen.Store(lease.Generation)
			if err := r.writeRunnerFile("generation", strconv.FormatInt(lease.Generation, 10)); err != nil {
				return fmt.Errorf("write generation file: %w", err)
			}
			r.leaseHeld.Store(true)
			if lease.TTLSeconds > 0 {
				r.cfg.LeaseTTL = time.Duration(lease.TTLSeconds) * time.Second
			}
			r.log.Info("lease acquired", "generation", lease.Generation, "expires_at", lease.ExpiresAt, "attempts", attempt)
			return nil
		}
		if stop.Err() != nil {
			return nil
		}
		if time.Now().After(deadline) {
			r.log.Error("giving up on the seat lease: it is still held by another holder or the platform is unreachable",
				"attempts", attempt, "waited", r.cfg.LeaseAcquireTimeout, "last_error", err)
			return fmt.Errorf("%w after %d attempts: %v", ErrLeaseTimeout, attempt, err)
		}
		level := slog.LevelWarn
		if errors.Is(err, client.ErrConflict) {
			level = slog.LevelInfo
		}
		r.log.Log(stop, level, "lease not acquired; retrying", "attempt", attempt, "err", err)
		select {
		case <-stop.Done():
			return nil
		case <-time.After(r.cfg.LeaseRetryInterval):
		}
	}
}

// renewLoop renews every RenewInterval. A fenced renewal, or no successful
// renewal within the lease TTL, means another holder may exist: fence.
func (r *Runner) renewLoop(done <-chan struct{}) {
	t := time.NewTicker(r.cfg.RenewInterval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-done:
			return
		case <-r.workCtx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(r.workCtx, r.cfg.RenewInterval)
		_, err := r.c.RenewLease(ctx, r.gen.Load())
		cancel()
		switch {
		case err == nil:
			last = time.Now()
		case client.IsFenced(err) || errors.Is(err, client.ErrNotFound) || errors.Is(err, client.ErrConflict):
			r.fence(err)
			return
		default:
			if time.Since(last) >= r.cfg.LeaseTTL {
				r.fence(fmt.Errorf("no successful renewal for %v: %w", time.Since(last).Round(time.Second), err))
				return
			}
			r.log.Warn("lease renewal failed; will retry", "err", err, "since_last_success", time.Since(last).Round(time.Millisecond))
		}
	}
}

func (r *Runner) release() {
	if r.fencedFlag.Load() || !r.leaseHeld.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.c.ReleaseLease(ctx, r.gen.Load()); err != nil {
		r.log.Warn("lease release failed", "err", err)
	} else {
		r.log.Info("lease released", "generation", r.gen.Load())
	}
	r.leaseHeld.Store(false)
}

func (r *Runner) writeRunnerFile(name, value string) error {
	if err := os.MkdirAll(r.cfg.RunnerDir, 0o755); err != nil {
		return err
	}
	return client.WriteFileAtomic(filepath.Join(r.cfg.RunnerDir, name), []byte(value+"\n"), 0o644)
}

// start loads the bootstrap and manifest, then prepares and starts the adapter.
func (r *Runner) start(stop context.Context) error {
	if err := ensureWritableDirs(r.cfg.WorkspaceDir, r.cfg.HomeDir, r.cfg.RunnerDir); err != nil {
		return err
	}
	boot, err := r.loadBootstrap(stop)
	if err != nil {
		return err
	}
	manifest, instructions := r.loadManifest()
	r.bootLoaded.Store(true)

	a, err := r.newAdapter(r.cfg.Harness)
	if err != nil {
		return err
	}
	fwd := newForwarder(r.c, r.log, a.Events(), r.fence)
	r.mu.Lock()
	r.adapter, r.boot, r.fwd = a, boot, fwd
	r.mu.Unlock()
	go fwd.run()

	env := r.environment(boot, manifest, instructions)
	r.mu.Lock()
	r.modelConn = env.ModelConnection
	r.mu.Unlock()
	pctx, cancel := context.WithTimeout(r.workCtx, 2*time.Minute)
	defer cancel()
	caps := a.DescribeCapabilities()
	r.log.Info("preparing harness", "adapter", caps.Adapter, "version", caps.Version, "interruption", caps.Interruption, "recovery_modes", caps.RecoveryModes)
	if err := a.Prepare(pctx, env); err != nil {
		return fmt.Errorf("prepare %s: %w", r.cfg.Harness, err)
	}
	res, err := a.StartOrResume(pctx, harnesses.RecoveryFromBootstrap(boot))
	if err != nil {
		return fmt.Errorf("start %s: %w", r.cfg.Harness, err)
	}
	r.log.Info("harness started", "mode", res.Mode, "session_id", res.SessionID, "note", res.Note)
	return nil
}

func (r *Runner) loadBootstrap(stop context.Context) (runtimeapi.Bootstrap, error) {
	deadline := time.Now().Add(r.cfg.BootstrapTimeout)
	for {
		ctx, cancel := context.WithTimeout(r.workCtx, 30*time.Second)
		b, err := r.c.Bootstrap(ctx)
		cancel()
		if err == nil {
			r.log.Info("bootstrap loaded", "seat", b.Self.SeatKey, "config_revision", b.Self.ConfigRevision, "policy_revision", b.Self.PolicyRevision,
				"pending_messages", b.Recovery.PendingMessages, "unknown_operations", len(b.Recovery.UnknownOperations))
			if b.Self.ConfigRevision != "" && r.cfg.ConfigRevision != "" && b.Self.ConfigRevision != r.cfg.ConfigRevision {
				r.log.Warn("platform config revision differs from the Pod's", "platform", b.Self.ConfigRevision, "pod", r.cfg.ConfigRevision)
			}
			return b, nil
		}
		if r.check(err) != nil && r.fencedFlag.Load() {
			return b, err
		}
		if time.Now().After(deadline) || stop.Err() != nil {
			return b, fmt.Errorf("load bootstrap: %w", err)
		}
		r.log.Warn("bootstrap not loaded; retrying", "err", err)
		select {
		case <-time.After(2 * time.Second):
		case <-stop.Done():
		case <-r.workCtx.Done():
		}
	}
}

// loadManifest reads the read-only manifest ConfigMap, if mounted.
func (r *Runner) loadManifest() (*compile.SeatManifest, string) {
	var m *compile.SeatManifest
	if b, err := os.ReadFile(filepath.Join(r.cfg.ManifestDir, "manifest.json")); err == nil {
		var sm compile.SeatManifest
		if err := json.Unmarshal(b, &sm); err != nil {
			r.log.Warn("manifest.json is invalid", "err", err)
		} else {
			m = &sm
			if sm.ConfigRevision != "" && r.cfg.ConfigRevision != "" && sm.ConfigRevision != r.cfg.ConfigRevision {
				r.log.Warn("manifest config revision differs from STEADMESH_CONFIG_REVISION", "manifest", sm.ConfigRevision, "env", r.cfg.ConfigRevision)
			}
		}
	} else {
		r.log.Warn("manifest not available", "dir", r.cfg.ManifestDir, "err", err)
	}
	// Bootstrap.Instructions carries the ordered instruction refs; the
	// rendered text comes only from the manifest ConfigMap.
	instr := ""
	if b, err := os.ReadFile(filepath.Join(r.cfg.ManifestDir, "instructions.md")); err == nil {
		instr = string(b)
	} else {
		r.log.Warn("instructions.md not available; the harness starts without instruction text", "dir", r.cfg.ManifestDir, "err", err)
	}
	return m, instr
}

func (r *Runner) environment(boot runtimeapi.Bootstrap, m *compile.SeatManifest, instructions string) harnesses.Environment {
	conn, model := r.cfg.ModelConnection, r.cfg.Model
	var hcfg map[string]string
	display := boot.Self.DisplayName
	if m != nil {
		if conn == "" {
			conn = m.Harness.ModelConnection
		}
		if model == "" {
			model = m.Harness.Model
		}
		hcfg = m.Harness.Config
		if display == "" {
			display = m.DisplayName
		}
	}
	env := harnesses.Environment{
		OrganizationID:  firstNonEmpty(r.cfg.OrgID, boot.Self.OrganizationID),
		SeatID:          firstNonEmpty(r.cfg.SeatID, boot.Self.SeatID),
		SeatKey:         firstNonEmpty(r.cfg.SeatKey, boot.Self.SeatKey),
		DisplayName:     display,
		ConfigRevision:  r.cfg.ConfigRevision,
		Generation:      r.gen.Load(),
		PlatformURL:     r.cfg.PlatformURL,
		TokenFile:       r.cfg.TokenFile,
		WorkspaceDir:    r.cfg.WorkspaceDir,
		HomeDir:         r.cfg.HomeDir,
		RunnerDir:       r.cfg.RunnerDir,
		TmpDir:          r.cfg.TmpDir,
		Instructions:    instructions,
		Bootstrap:       boot,
		ToolCommand:     r.cfg.ToolCommand,
		ModelConnection: conn,
		Model:           model,
		HarnessConfig:   hcfg,
		ExtraEnv:        r.toolEnv(),
	}
	if conn != "" {
		env.ModelProxyURL = r.c.ModelProxyURL(conn)
	}
	return env
}

// toolEnv is the environment steadmesh-tools needs inside harness processes.
func (r *Runner) toolEnv() []string {
	return []string{
		client.EnvPlatformURL + "=" + r.cfg.PlatformURL,
		client.EnvTokenFile + "=" + r.cfg.TokenFile,
		client.EnvRunnerDir + "=" + r.cfg.RunnerDir,
		client.EnvGeneration + "=" + strconv.FormatInt(r.gen.Load(), 10),
		"STEADMESH_ORG_ID=" + r.cfg.OrgID,
		"STEADMESH_SEAT_ID=" + r.cfg.SeatID,
		"STEADMESH_SEAT_KEY=" + r.cfg.SeatKey,
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// loop polls the inbox until stop or fencing.
func (r *Runner) loop(stop context.Context) error {
	pollCtx, cancelPoll := context.WithCancel(r.workCtx)
	defer cancelPoll()
	unhook := context.AfterFunc(stop, cancelPoll)
	defer unhook()
	failures := 0
	for {
		if stop.Err() != nil || r.workCtx.Err() != nil {
			return nil
		}
		d, err := r.c.InboxNext(pollCtx, r.cfg.PollWait)
		if err != nil {
			if pollCtx.Err() != nil {
				return nil
			}
			if r.check(err); r.fencedFlag.Load() {
				return nil
			}
			failures++
			backoff := min(time.Duration(failures)*time.Second, 15*time.Second)
			r.log.Warn("inbox poll failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-pollCtx.Done():
			}
			continue
		}
		failures = 0
		if d == nil {
			continue
		}
		if d.Message.Origin == "probe" {
			r.handleProbe(*d)
		} else {
			r.handleDelivery(stop, *d)
		}
	}
}

func (r *Runner) setExecution(id string) {
	if err := r.writeRunnerFile("execution", id); err != nil {
		r.log.Warn("write execution file failed", "err", err)
	}
}

func (r *Runner) handleDelivery(stop context.Context, d runtimeapi.InboxDelivery) {
	log := r.log.With("delivery_id", d.DeliveryID, "execution_id", d.ExecutionID, "message_id", d.Message.MessageID, "origin", d.Message.Origin, "attempt", d.Attempt)
	r.setExecution(d.ExecutionID)
	defer r.setExecution("")
	r.reportState(r.workCtx, StateExecuting, "message "+d.Message.MessageID)

	tctx, cancel := context.WithTimeout(r.workCtx, r.cfg.TurnTimeout)
	defer cancel()
	log.Info("turn started")
	tr, err := r.adapter.Deliver(tctx, harnesses.Delivery{DeliveryID: d.DeliveryID, ExecutionID: d.ExecutionID, Attempt: d.Attempt, Message: d.Message})
	fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
	r.fwd.Flush(fctx)
	fcancel()
	if r.fencedFlag.Load() {
		log.Warn("turn aborted by fencing; not acknowledging")
		return
	}
	if err != nil {
		// Not started (e.g. quiescing): leave the delivery for redelivery.
		log.Warn("turn not run; delivery left for redelivery", "err", err)
		return
	}
	log.Info("turn finished", "status", tr.Status, "error", tr.Error, "duration", tr.Duration, "session_id", tr.SessionID, "agent_claim", tr.Claim != nil)
	if tr.Recovery != nil {
		log.Warn("harness recovery", "mode", tr.Recovery.Mode, "note", tr.Recovery.Note)
	}

	ack := runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID}
	switch tr.Status {
	case harnesses.TurnCompleted:
		ack.Outcome = "completed"
	case harnesses.TurnInterrupted:
		// Interrupted by quiesce or stop: redeliver to the next execution.
		log.Warn("turn interrupted; delivery left for redelivery")
		r.checkpoint(r.workCtx)
		return
	default:
		ack.Outcome, ack.Error = "failed", harnesses.TruncateUTF8(tr.Status+": "+tr.Error, 2048)
	}
	actx, acancel := context.WithTimeout(r.workCtx, 15*time.Second)
	if err := r.check(r.c.Ack(actx, d.DeliveryID, ack)); err != nil {
		log.Warn("ack failed", "err", err)
	}
	acancel()
	r.checkpoint(r.workCtx)
	if stop.Err() == nil && !r.fencedFlag.Load() {
		r.reportState(r.workCtx, StateWarm, "")
	}
}

func (r *Runner) checkpoint(ctx context.Context) {
	if r.fencedFlag.Load() {
		return
	}
	r.mu.Lock()
	a := r.adapter
	r.mu.Unlock()
	if a == nil {
		return
	}
	cp, err := a.Checkpoint(ctx)
	if err != nil {
		r.log.Warn("checkpoint failed", "err", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := r.check(r.c.PutCheckpoint(cctx, cp)); err != nil {
		r.log.Warn("checkpoint upload failed", "err", err)
		return
	}
	r.log.Info("checkpoint", "adapter", cp.HarnessAdapter, "format", cp.FormatVersion, "ref", cp.CheckpointRef, "guarantee", cp.Guarantee)
}

// ensureWritableDirs creates the seat directories on the workspace volume and
// fails clearly when one is not writable (for example, a directory created by
// root on a volume that ignores fsGroup), instead of letting every turn fail.
func ensureWritableDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("seat directory %s: %w", d, err)
		}
		f, err := os.CreateTemp(d, ".write-check-*")
		if err != nil {
			return fmt.Errorf("seat directory %s is not writable by uid %d: %w", d, os.Getuid(), err)
		}
		f.Close()
		os.Remove(f.Name())
	}
	return nil
}
