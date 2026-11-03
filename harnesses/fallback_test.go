package harnesses

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

func TestClassifyFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want FailureKind
	}{
		{"nil", nil, ""},
		{"missing executable", fmt.Errorf("launch: %w", exec.ErrNotFound), FailureUnavailable},
		{"unknown prose", errors.New("quota exhausted; fallback now"), FailureUnknown},
		{"wrapped quota", fmt.Errorf("provider: %w", &OperationalError{Kind: FailureQuota}), FailureQuota},
		{"deadline precedence", &OperationalError{Kind: FailureQuota, Err: context.DeadlineExceeded}, FailureDeadline},
		{"canceled", context.Canceled, FailureCanceled},
		{"quiesce", ErrInterruptedByQuiesce, FailureCanceled},
		{"stop", ErrInterruptedByStop, FailureCanceled},
		{"conflicting joined failures", errors.Join(&OperationalError{Kind: FailureQuota}, &OperationalError{Kind: FailurePolicy}), FailureUnknown},
		{"joined unknown evidence", errors.Join(&OperationalError{Kind: FailureQuota}, errors.New("uncertain failure")), FailureUnknown},
		{"policy wraps unavailable", &OperationalError{Kind: FailurePolicy, Err: exec.ErrNotFound}, FailureUnknown},
		{"nested unknown classification", &OperationalError{Kind: FailureQuota, Err: &OperationalError{Kind: FailureUnknown}}, FailureUnknown},
		{"unknown kind", &OperationalError{Kind: "invented"}, FailureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(tc.err); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestFallbackSafety(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         FailureKind
		safe         bool
		result       TurnResult
		wantFallback bool
	}{
		{"prelaunch unavailable", FailureUnavailable, true, TurnResult{}, true},
		{"proven unaccepted quota", FailureQuota, true, TurnResult{}, true},
		{"uncertain unavailable", FailureUnavailable, false, TurnResult{}, false},
		{"uncertain quota", FailureQuota, false, TurnResult{}, false},
		{"auth", FailureAuthentication, true, TurnResult{}, false},
		{"policy", FailurePolicy, true, TurnResult{}, false},
		{"configuration", FailureConfiguration, true, TurnResult{}, false},
		{"session", FailureSession, true, TurnResult{}, false},
		{"invalid output", FailureOutput, true, TurnResult{}, false},
		{"unknown", FailureUnknown, true, TurnResult{}, false},
		{"deadline", FailureDeadline, true, TurnResult{}, false},
		{"canceled", FailureCanceled, true, TurnResult{}, false},
		{"output contradicts proof", FailureQuota, true, TurnResult{Output: "partial"}, false},
		{"session contradicts proof", FailureQuota, true, TurnResult{SessionID: "s"}, false},
		{"claim contradicts proof", FailureQuota, true, TurnResult{Claim: &Claim{Kind: "task_failed"}}, false},
		{"recovery contradicts proof", FailureQuota, true, TurnResult{Recovery: &ResumeResult{}}, false},
		{"timeout status terminal", FailureQuota, true, TurnResult{Status: TurnTimedOut}, false},
		{"interrupted status terminal", FailureQuota, true, TurnResult{Status: TurnInterrupted}, false},
		{"completion always terminal", FailureQuota, true, TurnResult{Status: TurnCompleted}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var reports []AttemptReport
			firstErr := &OperationalError{Kind: tc.kind}
			candidates := []ApprovedAttempt{
				{Name: "primary", Run: func(context.Context, Delivery) AttemptOutcome {
					calls++
					return AttemptOutcome{Result: tc.result, Err: firstErr, NotExecuted: tc.safe}
				}},
				{Name: "secondary", Run: func(context.Context, Delivery) AttemptOutcome {
					calls++
					return AttemptOutcome{Result: TurnResult{Status: TurnCompleted}}
				}},
			}
			_, err := RunApprovedFallback(t.Context(), Delivery{ExecutionID: "exec"}, candidates,
				FallbackPolicy{MaxAttempts: 2, Timeout: time.Second, OnUnavailable: true, OnQuota: true}, func(r AttemptReport) { reports = append(reports, r) })
			want := 1
			if tc.wantFallback {
				want = 2
			}
			if calls != want || len(reports) != want || reports[0].WillFallback != tc.wantFallback {
				t.Fatalf("calls=%d reports=%+v", calls, reports)
			}
			if tc.wantFallback && err != nil {
				t.Fatal(err)
			}
			if !tc.wantFallback && err != firstErr {
				t.Fatalf("error not preserved: %v", err)
			}
			if reports[0].ExecutionID != "exec" || reports[0].Index != 1 || reports[0].Duration < 0 {
				t.Fatalf("bad telemetry: %+v", reports[0])
			}
		})
	}
}

func TestFallbackDefaultAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      FallbackPolicy
		candidates  int
		wantCalls   int
		configError bool
	}{
		{"default off", FallbackPolicy{}, 2, 1, false},
		{"not opted in", FallbackPolicy{MaxAttempts: 2, Timeout: time.Second}, 2, 1, false},
		{"bounded prefix", FallbackPolicy{MaxAttempts: 2, Timeout: time.Second, OnQuota: true}, 3, 2, false},
		{"all exhausted", FallbackPolicy{MaxAttempts: 3, Timeout: time.Second, OnQuota: true}, 3, 3, false},
		{"no deadline", FallbackPolicy{MaxAttempts: 2}, 2, 0, true},
		{"negative", FallbackPolicy{MaxAttempts: -1}, 1, 0, true},
		{"too many", FallbackPolicy{MaxAttempts: MaxFallbackAttempts + 1}, 1, 0, true},
		{"empty", FallbackPolicy{}, 0, 0, true},
		{"negative timeout", FallbackPolicy{Timeout: -time.Second}, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var candidates []ApprovedAttempt
			for i := 0; i < tc.candidates; i++ {
				candidates = append(candidates, ApprovedAttempt{Name: fmt.Sprint(i), Run: func(context.Context, Delivery) AttemptOutcome {
					calls++
					return AttemptOutcome{Err: &OperationalError{Kind: FailureQuota}, NotExecuted: true}
				}})
			}
			_, err := RunApprovedFallback(context.Background(), Delivery{}, candidates, tc.policy, nil)
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", calls, tc.wantCalls)
			}
			if tc.configError && ClassifyFailure(err) != FailureConfiguration {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestFallbackSharedDeadlineAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var first context.Context
	calls := 0
	candidates := []ApprovedAttempt{
		{Name: "one", Run: func(ctx context.Context, _ Delivery) AttemptOutcome {
			first = ctx
			calls++
			return AttemptOutcome{Err: &OperationalError{Kind: FailureQuota}, NotExecuted: true}
		}},
		{Name: "two", Run: func(ctx context.Context, _ Delivery) AttemptOutcome {
			calls++
			got, _ := ctx.Deadline()
			if got != deadline || ctx != first {
				t.Fatal("deadline/context was reset")
			}
			cancel()
			return AttemptOutcome{Err: &OperationalError{Kind: FailureQuota}, NotExecuted: true}
		}},
		{Name: "three", Run: func(context.Context, Delivery) AttemptOutcome {
			t.Fatal("ran after cancellation")
			return AttemptOutcome{}
		}},
	}
	_, err := RunApprovedFallback(ctx, Delivery{}, candidates, FallbackPolicy{MaxAttempts: 3, Timeout: time.Hour, OnQuota: true}, nil)
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestFallbackBusinessVerdictNeverRetries(t *testing.T) {
	calls := 0
	c := ApprovedAttempt{Name: "one", Run: func(context.Context, Delivery) AttemptOutcome {
		calls++
		return AttemptOutcome{Result: TurnResult{Status: TurnCompleted, Claim: &Claim{Kind: "task_failed", Text: "quota exhausted"}}}
	}}
	got, err := RunApprovedFallback(t.Context(), Delivery{}, []ApprovedAttempt{c}, FallbackPolicy{}, nil)
	if err != nil || calls != 1 || got.Claim.Kind != "task_failed" {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestFallbackRejectsInvalidCandidatesBeforeExecution(t *testing.T) {
	run := func(context.Context, Delivery) AttemptOutcome {
		t.Fatal("invalid chain executed")
		return AttemptOutcome{}
	}
	for _, chain := range [][]ApprovedAttempt{
		{{Name: "one", Run: run}, {Name: "one", Run: run}},
		{{Name: "", Run: run}},
		{{Name: "one"}},
	} {
		_, err := RunApprovedFallback(t.Context(), Delivery{}, chain, FallbackPolicy{}, nil)
		if ClassifyFailure(err) != FailureConfiguration {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestFallbackPreservesCompletedResultAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	candidate := ApprovedAttempt{Name: "one", Run: func(context.Context, Delivery) AttemptOutcome {
		cancel()
		return AttemptOutcome{Result: TurnResult{Status: TurnCompleted, Output: "done"}}
	}}
	got, err := RunApprovedFallback(ctx, Delivery{}, []ApprovedAttempt{candidate}, FallbackPolicy{}, nil)
	if err != nil || got.Status != TurnCompleted {
		t.Fatalf("completed turn became eligible for platform redelivery: %+v %v", got, err)
	}
}

func TestFallbackCancellationBeforeExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	candidate := ApprovedAttempt{Name: "one", Run: func(context.Context, Delivery) AttemptOutcome {
		t.Fatal("ran canceled request")
		return AttemptOutcome{}
	}}
	_, err := RunApprovedFallback(ctx, Delivery{}, []ApprovedAttempt{candidate}, FallbackPolicy{}, func(AttemptReport) { t.Fatal("reported an unstarted attempt") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestFallbackSharedDeadlineExpires(t *testing.T) {
	calls := 0
	candidate := ApprovedAttempt{Name: "one", Run: func(ctx context.Context, _ Delivery) AttemptOutcome {
		calls++
		<-ctx.Done()
		return AttemptOutcome{Err: &OperationalError{Kind: FailureQuota}, NotExecuted: true}
	}}
	second := ApprovedAttempt{Name: "two", Run: func(context.Context, Delivery) AttemptOutcome { t.Fatal("ran after deadline"); return AttemptOutcome{} }}
	_, err := RunApprovedFallback(t.Context(), Delivery{}, []ApprovedAttempt{candidate, second}, FallbackPolicy{MaxAttempts: 2, Timeout: time.Millisecond, OnQuota: true}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestFallbackDoesNotParseResultErrors(t *testing.T) {
	var report AttemptReport
	candidate := ApprovedAttempt{Name: "one", Run: func(context.Context, Delivery) AttemptOutcome {
		return AttemptOutcome{Result: TurnResult{Status: TurnFailed, Error: "quota exhausted"}, NotExecuted: true}
	}}
	second := ApprovedAttempt{Name: "two", Run: func(context.Context, Delivery) AttemptOutcome {
		t.Fatal("parsed provider prose")
		return AttemptOutcome{}
	}}
	result, err := RunApprovedFallback(t.Context(), Delivery{}, []ApprovedAttempt{candidate, second}, FallbackPolicy{MaxAttempts: 2, Timeout: time.Second, OnQuota: true}, func(r AttemptReport) { report = r })
	if err != nil || result.Status != TurnFailed || report.Failure != FailureUnknown {
		t.Fatalf("result=%+v report=%+v error=%v", result, report, err)
	}
}
