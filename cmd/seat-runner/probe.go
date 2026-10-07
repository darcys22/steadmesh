package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

// handleProbe runs the readiness probe (contracts.md, Probe messages). It
// always checks a workspace write and read. A harness with a model must prove
// itself with a real turn: the model is reached through the platform proxy
// (model), the harness calls the platform `self` tool through its tool
// bridge (tool), and the turn completes (turn). A connection probe alone is
// never evidence that a harness works. A harness without a model calls
// `self` through the steadmesh-tools binary instead. Probes create no
// business work.
func (r *Runner) handleProbe(d runtimeapi.InboxDelivery) {
	log := r.log.With("delivery_id", d.DeliveryID, "execution_id", d.ExecutionID, "message_id", d.Message.MessageID)
	r.setExecution(d.ExecutionID)
	defer r.setExecution("")
	ctx, cancel := context.WithTimeout(r.workCtx, 2*time.Minute)
	defer cancel()

	checks := map[string]string{}
	checks["workspace"] = result(r.probeWorkspace(d.ExecutionID))
	r.mu.Lock()
	model := r.model
	r.mu.Unlock()
	if model != nil {
		m, tool, turn := r.probeHarness(ctx, d)
		checks["model"], checks["tool"], checks["turn"] = result(m), result(tool), result(turn)
	} else {
		checks["tool"] = result(r.probeTool(ctx, d.ExecutionID))
	}
	ok := true
	var failed []string
	for k, v := range checks {
		if v != "ok" {
			ok = false
			failed = append(failed, k+": "+v)
		}
	}
	sort.Strings(failed)
	pr := runtimeapi.ProbeResult{OK: ok, Checks: checks}
	ev := runtimeapi.ExecutionEvent{Kind: runtimeapi.EventProbeResult, Time: time.Now().UTC(), Data: harnesses.MustJSON(pr)}
	pctx, pcancel := context.WithTimeout(r.workCtx, 15*time.Second)
	if err := r.check(r.c.PostEvents(pctx, d.ExecutionID, []runtimeapi.ExecutionEvent{ev})); err != nil {
		log.Warn("posting probe result failed", "err", err)
	}
	pcancel()
	// The probe verdict is carried by data.checks; the delivery itself was
	// handled, so it is always acknowledged as completed.
	ack := runtimeapi.InboxAckRequest{ExecutionID: d.ExecutionID, Outcome: "completed"}
	actx, acancel := context.WithTimeout(r.workCtx, 15*time.Second)
	if err := r.check(r.c.Ack(actx, d.DeliveryID, ack)); err != nil {
		log.Warn("probe ack failed", "err", err)
	}
	acancel()
	log.Info("probe handled", "ok", ok, "checks", checks, "failed", strings.Join(failed, "; "))
}

func result(err error) string {
	if err == nil {
		return "ok"
	}
	return "fail: " + harnesses.TruncateUTF8(err.Error(), 512)
}

func (r *Runner) probeTool(ctx context.Context, executionID string) error {
	cmd := exec.CommandContext(ctx, r.cfg.ToolCommand, "call", "self", "{}")
	cmd.Env = append(os.Environ(), r.toolEnv()...)
	cmd.Env = append(cmd.Env, client.EnvExecution+"="+executionID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("steadmesh-tools call self: %v: %s%s", err, strings.TrimSpace(stderr.String()), harnesses.TruncateUTF8(strings.TrimSpace(stdout.String()), 200))
	}
	return nil
}

func (r *Runner) probeWorkspace(executionID string) error {
	dir := filepath.Join(r.cfg.RunnerDir, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	want := hex.EncodeToString(nonce)
	name := url.PathEscape(executionID)
	if name == "" {
		name = "probe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	defer os.Remove(p)
	got, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if string(got) != want {
		return errors.New("read back different content")
	}
	return nil
}

// probeHarness delivers harnesses.ProbePrompt to the harness and reports the model,
// tool and turn checks from what the runner observed.
func (r *Runner) probeHarness(ctx context.Context, d runtimeapi.InboxDelivery) (model, tool, turn error) {
	r.mu.Lock()
	a, fwd := r.adapter, r.fwd
	r.mu.Unlock()
	tr, stop := r.trace(d.ExecutionID)
	defer stop()
	msg := d.Message
	msg.Body = harnesses.ProbePrompt
	res, err := a.Deliver(ctx, harnesses.Delivery{DeliveryID: d.DeliveryID, ExecutionID: d.ExecutionID, Attempt: d.Attempt, Message: msg})
	// Events emitted before Deliver returned are observed once flushed.
	fctx, cancel := context.WithTimeout(r.workCtx, 10*time.Second)
	fwd.Flush(fctx)
	cancel()
	t := r.snapshot(tr)

	switch {
	case err != nil:
		turn = err
	case res.Status != harnesses.TurnCompleted:
		turn = fmt.Errorf("turn %s: %s", res.Status, res.Error)
	}
	ok := false
	for _, s := range t.modelStatuses {
		ok = ok || s/100 == 2
	}
	switch {
	case ok:
	case len(t.modelStatuses) == 0:
		model = errors.New("the harness made no model request")
	default:
		model = fmt.Errorf("no model request succeeded (statuses %v)", t.modelStatuses)
	}
	called := false
	for corr, name := range t.toolCalls {
		if isSelfTool(name) {
			if t.toolErrors[corr] {
				tool = fmt.Errorf("the self tool call (%s) failed", name)
			}
			called = true
		}
	}
	if !called {
		names := make([]string, 0, len(t.toolCalls))
		for _, n := range t.toolCalls {
			names = append(names, n)
		}
		sort.Strings(names)
		tool = fmt.Errorf("the harness did not call the self tool (tool calls: %v)", names)
	}
	return model, tool, turn
}

// isSelfTool matches the platform self tool as harnesses name it:
// self, mcp__steadmesh__self, or a namespaced form ending in "self".
func isSelfTool(name string) bool {
	if name == "self" {
		return true
	}
	return strings.HasSuffix(name, "__self") || strings.HasSuffix(name, "/self") || strings.HasSuffix(name, ".self")
}
