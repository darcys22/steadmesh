package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// handleProbe runs the synthetic readiness probe (contracts.md, Probe
// messages) without involving the harness: a tool call through the
// steadmesh-tools binary, a workspace write/read, and a model-proxy GET
// /v1/models when a model connection exists. It creates no business work.
func (r *Runner) handleProbe(d runtimeapi.InboxDelivery) {
	log := r.log.With("delivery_id", d.DeliveryID, "execution_id", d.ExecutionID, "message_id", d.Message.MessageID)
	r.setExecution(d.ExecutionID)
	defer r.setExecution("")
	ctx, cancel := context.WithTimeout(r.workCtx, 2*time.Minute)
	defer cancel()

	checks := map[string]string{}
	checks["tool"] = result(r.probeTool(ctx, d.ExecutionID))
	checks["workspace"] = result(r.probeWorkspace(d.ExecutionID))
	r.mu.Lock()
	conn := r.modelConn
	r.mu.Unlock()
	if conn != "" {
		checks["model"] = result(r.probeModel(ctx, conn))
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

func (r *Runner) probeModel(ctx context.Context, conn string) error {
	path := runtimeapi.PathModelProxy + url.PathEscape(conn) + "/v1/models"
	resp, err := r.c.Raw(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
