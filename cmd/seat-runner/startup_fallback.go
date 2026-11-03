package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/modelforward"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// approvedStartupCandidates chooses only manifest-approved routes. A persisted
// native session is sticky: missing/revoked/changed routes never fall back.
func approvedStartupCandidates(m *compile.SeatManifest, cp *runtimeapi.Checkpoint) ([]compile.StartupFallbackCandidate, time.Duration, error) {
	if m == nil {
		return nil, 0, errors.New("startup route requires a mounted manifest")
	}
	if m.Harness.StartupFallback == nil && len(m.StartupFallbacks) != 0 {
		return nil, 0, errors.New("startup candidates require an explicit fallback policy")
	}
	candidates := []compile.StartupFallbackCandidate{{Profile: m.HarnessProfile, Harness: m.Harness}}
	candidates = append(candidates, m.StartupFallbacks...)
	timeout := 2 * time.Minute
	if p := m.Harness.StartupFallback; p != nil {
		var err error
		timeout, err = time.ParseDuration(p.TotalTimeout)
		if err != nil || timeout <= 0 || timeout > 5*time.Minute || p.MaxAttempts < 1 || p.MaxAttempts > harnesses.MaxFallbackAttempts || p.MaxAttempts > len(candidates) || len(candidates) > harnesses.MaxFallbackAttempts {
			return nil, 0, errors.New("invalid startup fallback manifest")
		}
		if cp == nil {
			candidates = candidates[:p.MaxAttempts]
		}
	}
	if cp != nil {
		if cp.HarnessProfile == "" || cp.RouteBinding == "" {
			return nil, 0, errors.New("startup fallback cannot migrate a legacy checkpoint; explicit session migration is required")
		}
		for _, candidate := range candidates {
			if candidate.Profile == cp.HarnessProfile {
				if candidate.Harness.Adapter != cp.HarnessAdapter || compile.StartupCandidateBinding(candidate.Profile, candidate.Harness) != cp.RouteBinding {
					return nil, 0, errors.New("persisted startup route binding no longer matches the approved profile")
				}
				return []compile.StartupFallbackCandidate{candidate}, timeout, nil
			}
		}
		return nil, 0, errors.New("persisted startup route is no longer approved")
	}
	return candidates, timeout, nil
}

func (r *Runner) startApprovedRoute(stop context.Context, boot runtimeapi.Bootstrap, m *compile.SeatManifest, instructions string) error {
	candidates, timeout, err := approvedStartupCandidates(m, boot.Recovery.Session)
	if err != nil {
		return err
	}
	if m.Harness.Adapter != r.cfg.Harness {
		return errors.New("startup route manifest does not match the pod harness")
	}
	if (m.ConfigRevision != "" && boot.Self.ConfigRevision != "" && m.ConfigRevision != boot.Self.ConfigRevision) || (r.cfg.ConfigRevision != "" && boot.Self.ConfigRevision != "" && r.cfg.ConfigRevision != boot.Self.ConfigRevision) {
		return errors.New("startup route requires a current manifest revision")
	}
	if len(boot.Recovery.UnknownOperations) > 0 && len(candidates) > 1 {
		return errors.New("startup fallback refused while operation outcomes are unknown")
	}
	ctx, cancel := context.WithTimeout(r.workCtx, timeout)
	defer cancel()
	// SIGTERM before the delivery loop must also stop the startup chain.
	stopCancel := context.AfterFunc(stop, cancel)
	defer stopCancel()
	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.fencedFlag.Load() {
			return ErrFenced
		}
		binding := compile.StartupCandidateBinding(candidate.Profile, candidate.Harness)
		a, err := r.newAdapter(candidate.Harness.Adapter)
		if err != nil {
			return err
		} // registration/configuration errors never switch
		r.mu.Lock()
		r.model = candidate.Harness.Model
		r.mu.Unlock()
		if candidate.Harness.Model != nil && r.mfwd == nil {
			mf, err := modelforward.Start(modelforward.Options{PlatformURL: r.cfg.PlatformURL, TokenFile: r.cfg.TokenFile, Generation: r.gen.Load, Execution: r.currentExecution, Observe: r.observeModel, Log: r.log})
			if err != nil {
				return err
			}
			r.mu.Lock()
			r.mfwd = mf
			r.mu.Unlock()
		}
		fwd := newForwarder(r.c, r.log, a.Events(), r.fence)
		fwd.observe = r.observeEvent
		r.mu.Lock()
		r.adapter, r.boot, r.fwd = a, boot, fwd
		r.mu.Unlock()
		go fwd.run()
		selected := *m
		selected.HarnessProfile, selected.Harness = candidate.Profile, candidate.Harness
		env := r.environment(boot, &selected, instructions)
		// Separate persistent harness state for profiles sharing the same adapter.
		env.RunnerDir = filepath.Join(env.RunnerDir, "routes", binding)
		env.HomeDir = filepath.Join(env.HomeDir, "routes", binding)
		start := time.Now()
		err = a.Prepare(ctx, env)
		if err != nil {
			next := errors.Is(err, exec.ErrNotFound) && harnesses.ClassifyFailure(err) == harnesses.FailureUnavailable && ctx.Err() == nil && boot.Recovery.Session == nil && index+1 < len(candidates)
			r.log.Warn("harness startup attempt finished", "profile", candidate.Profile, "attempt_index", index+1, "failure_kind", harnesses.ClassifyFailure(err), "duration", time.Since(start), "will_fallback", next)
			// A missing executable during Prepare precedes StartOrResume/Deliver.
			// No other error is proof of non-execution, even if its text says quota.
			if !next {
				return fmt.Errorf("prepare approved profile %s: %w", candidate.Profile, err)
			}
			if err := a.Stop(ctx); err != nil {
				return fmt.Errorf("stop failed startup candidate: %w", err)
			}
			fwd.Wait(ctx)
			select {
			case <-fwd.done:
			default:
				return errors.New("startup candidate event stream did not stop")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.StartOrResume(ctx, harnesses.RecoveryFromBootstrap(boot))
		if err != nil {
			return fmt.Errorf("start approved profile %s: %w", candidate.Profile, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cp, err := a.Checkpoint(ctx)
		if err != nil {
			return fmt.Errorf("checkpoint startup route: %w", err)
		}
		cp.HarnessProfile, cp.RouteBinding = candidate.Profile, binding
		// Publish the durable, generation-fenced route before accepting a message.
		// A failed/uncertain write is terminal; no other candidate is started.
		if err := r.check(r.c.PutCheckpoint(ctx, cp)); err != nil {
			return fmt.Errorf("persist startup route: %w", err)
		}
		r.mu.Lock()
		r.activeProfile, r.activeBinding = candidate.Profile, binding
		r.mu.Unlock()
		r.log.Info("approved harness route started", "profile", candidate.Profile, "adapter", candidate.Harness.Adapter, "attempt_index", index+1, "mode", res.Mode, "session_id", res.SessionID, "duration", time.Since(start))
		return nil
	}
	return errors.New("startup fallback candidates exhausted")
}
