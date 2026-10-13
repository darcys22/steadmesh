package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/modelforward"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/taskworker"
)

func taskWorkerMain() int {
	cfg, err := ConfigFromEnv()
	if err != nil {
		slog.Error("worker config", "error", err)
		return exitError
	}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	r, err := NewRunner(cfg, slog.Default(), DefaultAdapters)
	if err != nil {
		slog.Error("worker client", "error", err)
		return exitError
	}
	if err = r.runTaskWorker(stop, os.Getenv("STEADMESH_TASK_RUN_ID")); err != nil {
		slog.Error("task worker failed", "error", err)
		return exitError
	}
	return exitOK
}

// runTaskWorker executes one fresh turn. It does not acquire/renew/release a
// seat lease, fetch parent recovery, checkpoint or run the parent's inbox loop.
func (r *Runner) runTaskWorker(ctx context.Context, id string) (retErr error) {
	var boot taskworker.Bootstrap
	claim, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := r.c.Do(claim, http.MethodPost, runtimeapi.PathTaskWorker+"/claim", nil, struct{}{}, &boot)
	cancel()
	if err != nil {
		return err
	}
	if id == "" || boot.Run.ID != id {
		return errors.New("task worker identity mismatch")
	}
	taskCtx, cancel := context.WithDeadline(ctx, boot.Run.Deadline)
	defer cancel()
	result := runtimeapi.TaskRunFinish{State: runtimeapi.TaskFailed}
	defer func() {
		finish, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := r.c.Do(finish, http.MethodPost, runtimeapi.PathTaskWorker+"/finish", nil, result, nil); err != nil {
			r.log.Error("report worker result", "error", err)
			if retErr == nil {
				retErr = fmt.Errorf("return worker result: %w", err)
			}
		}
	}()
	fail := func(err error) error {
		result.Result.Error = harnesses.TruncateUTF8(err.Error(), 2048)
		if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			result.State = runtimeapi.TaskTimedOut
		}
		return err
	}
	for _, dir := range []string{r.cfg.WorkspaceDir, r.cfg.HomeDir, r.cfg.RunnerDir, r.cfg.TmpDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fail(err)
		}
	}
	if err := writeTaskInputs(r.cfg.WorkspaceDir, boot.Run.Request.InputFiles); err != nil {
		return fail(err)
	}
	r.gen.Store(boot.Run.ParentGeneration)
	r.execution.Store(boot.Run.ParentExecutionID)
	r.model = boot.Manifest.Harness.Model
	if r.model != nil {
		r.mfwd, err = modelforward.Start(modelforward.Options{PlatformURL: r.cfg.PlatformURL, TokenFile: r.cfg.TokenFile, Generation: r.gen.Load, Execution: r.currentExecution, Log: r.log})
		if err != nil {
			return fail(err)
		}
		defer r.mfwd.Close()
	}
	a, err := r.newAdapter(boot.Manifest.Harness.Adapter)
	if err != nil {
		return fail(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range a.Events() {
		}
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Stop(c)
		<-drained
	}
	defer stop()
	env := r.environment(boot.Identity, &boot.Manifest, "")
	if err = a.Prepare(taskCtx, env); err != nil {
		return fail(err)
	}
	if _, err = a.StartOrResume(taskCtx, harnesses.RecoveryDescriptor{}); err != nil {
		return fail(err)
	}
	turn, err := a.Deliver(taskCtx, harnesses.Delivery{ExecutionID: boot.Run.ID, Attempt: 1, Timezone: boot.Manifest.Timezone,
		Message: runtimeapi.Envelope{MessageID: boot.Run.ID, OrganizationID: r.cfg.OrgID, Origin: "seat", SenderSeat: r.cfg.SeatKey, SenderSeatID: r.cfg.SeatID, Body: boot.Run.Request.Prompt, CorrelationID: boot.Run.ParentExecutionID, CreatedAt: time.Now()}})
	stop() // Artifact collection happens only after all harness processes stop.
	if err != nil {
		return fail(err)
	}
	if turn.Status != harnesses.TurnCompleted {
		return fail(fmt.Errorf("harness turn %s: %s", turn.Status, turn.Error))
	}
	artifacts, err := readTaskArtifacts(r.cfg.WorkspaceDir, boot.Run.Request.ArtifactPaths)
	if err != nil {
		return fail(err)
	}
	result.State = runtimeapi.TaskCompleted
	result.Result = runtimeapi.TaskRunResult{Output: harnesses.TruncateUTF8(turn.Output, runtimeapi.MaxTaskOutputBytes), Artifacts: artifacts}
	return nil
}

func writeTaskInputs(dir string, files []runtimeapi.TaskFile) error {
	if err := runtimeapi.ValidateTaskFiles(files); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range files {
		if err := root.MkdirAll(path.Dir(f.Path), 0700); err != nil {
			return err
		}
		file, err := root.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.WriteString(f.Content)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func readTaskArtifacts(dir string, paths []string) ([]runtimeapi.TaskFile, error) {
	if len(paths) > runtimeapi.MaxTaskFiles {
		return nil, errors.New("too many artifact paths")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	out := []runtimeapi.TaskFile{}
	remaining := runtimeapi.MaxTaskFileBytes
	for _, p := range paths {
		if !runtimeapi.ValidTaskPath(p) {
			return nil, errors.New("invalid artifact path")
		}
		// OpenRoot confines path resolution, including symlinks and concurrent
		// renames. Lstat also rejects links, FIFOs and devices as return artifacts.
		info, err := root.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact %s is not a regular file", p)
		}
		f, err := root.Open(p)
		if err != nil {
			return nil, err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, int64(remaining+1)))
		f.Close()
		if readErr != nil {
			return nil, readErr
		}
		remaining -= len(b) + len(p)
		if remaining < 0 {
			return nil, errors.New("artifacts exceed byte limit")
		}
		out = append(out, runtimeapi.TaskFile{Path: p, Content: string(b)})
	}
	if err := runtimeapi.ValidateTaskFiles(out); err != nil {
		return nil, err
	}
	return out, nil
}
