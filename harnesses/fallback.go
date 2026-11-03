package harnesses

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaxFallbackAttempts is a hard bound including the primary attempt.
const MaxFallbackAttempts = 8

// AttemptOutcome keeps proof of replay safety separate from failure type.
// NotExecuted must only be true when the adapter can prove no prompt was
// accepted, no session was mutated and no tool or other side effect occurred.
// A timeout, missing response, process exit, or quota error alone is not proof.
type AttemptOutcome struct {
	Result      TurnResult
	Err         error
	NotExecuted bool
}

// ApprovedAttempt is a trusted, already-authorized invocation. The caller owns
// lifecycle, endpoint authorization, credentials, fencing and checkpointing.
// RunApprovedFallback never constructs adapters, switches model endpoints or
// transfers native sessions. Each callback must honor context cancellation and
// return only once its invocation has stopped.
type ApprovedAttempt struct {
	Name string
	Run  func(context.Context, Delivery) AttemptOutcome
}

// FallbackPolicy is platform-owned, never populated from agent output.
// A zero value disables fallback (one attempt). Timeout bounds the whole chain,
// not each attempt; an earlier parent deadline always wins.
type FallbackPolicy struct {
	MaxAttempts   int
	Timeout       time.Duration
	OnUnavailable bool
	OnQuota       bool
}

// AttemptReport is safe operational telemetry: no prompts, output, tokens,
// credentials or raw provider error strings. Index is one-based within this
// delivery and is separate from Delivery.Attempt (the platform redelivery).
type AttemptReport struct {
	Index        int           `json:"index"`
	Name         string        `json:"name"`
	ExecutionID  string        `json:"execution_id"`
	MessageID    string        `json:"message_id"`
	Duration     time.Duration `json:"duration"`
	Status       string        `json:"status"`
	Failure      FailureKind   `json:"failure,omitempty"`
	NotExecuted  bool          `json:"not_executed"`
	WillFallback bool          `json:"will_fallback"`
}

// RunApprovedFallback tries each explicitly approved candidate at most once,
// in order. A successful technical completion ends the chain regardless of any
// business claim. Session recovery remains with the owning adapter.
// observe is synchronous and must not block. All attempts share one context.
func RunApprovedFallback(ctx context.Context, d Delivery, candidates []ApprovedAttempt, policy FallbackPolicy, observe func(AttemptReport)) (TurnResult, error) {
	limit := policy.MaxAttempts
	if limit == 0 {
		limit = 1
	}
	if limit < 1 || limit > MaxFallbackAttempts || policy.Timeout < 0 || len(candidates) == 0 || len(candidates) > MaxFallbackAttempts {
		return TurnResult{}, &OperationalError{Kind: FailureConfiguration, Err: errors.New("invalid bounded fallback policy")}
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.Name == "" || c.Run == nil || seen[c.Name] {
			return TurnResult{}, &OperationalError{Kind: FailureConfiguration, Err: fmt.Errorf("invalid or duplicate fallback candidate %q", c.Name)}
		}
		seen[c.Name] = true
	}
	if policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, policy.Timeout)
		defer cancel()
	}
	if _, bounded := ctx.Deadline(); limit > 1 && !bounded {
		return TurnResult{}, &OperationalError{Kind: FailureConfiguration, Err: errors.New("fallback requires a shared deadline")}
	}
	var out AttemptOutcome
	for i, candidate := range candidates {
		if i >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return out.Result, err
		}
		start := time.Now()
		out = candidate.Run(ctx, d)
		failure := ClassifyFailure(out.Err)
		if err := ctx.Err(); err != nil && out.Err != nil {
			out.Err, failure = err, ClassifyFailure(err)
		}
		status := out.Result.Status
		if status == "" && out.Err != nil {
			status = TurnFailed
		}
		switch status {
		case TurnTimedOut:
			failure = FailureDeadline
		case TurnInterrupted:
			failure = FailureCanceled
		default:
			if failure == "" && status != TurnCompleted {
				failure = FailureUnknown
			}
		}
		allowed := (failure == FailureUnavailable && policy.OnUnavailable) || (failure == FailureQuota && policy.OnQuota)
		// Defense in depth: visible output, native session or recovery metadata
		// contradicts a pre-execution assertion, so it must never be replayed.
		safe := out.NotExecuted && out.Result.Output == "" && out.Result.SessionID == "" && out.Result.Claim == nil && out.Result.Recovery == nil
		next := out.Err != nil && status != TurnCompleted && allowed && safe && ctx.Err() == nil && i+1 < limit && i+1 < len(candidates)
		if observe != nil {
			observe(AttemptReport{Index: i + 1, Name: candidate.Name, ExecutionID: d.ExecutionID, MessageID: d.Message.MessageID,
				Duration: time.Since(start), Status: status, Failure: failure, NotExecuted: out.NotExecuted, WillFallback: next})
		}
		if !next {
			return out.Result, out.Err
		}
	}
	return out.Result, out.Err
}
