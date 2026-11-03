package harnesses

import (
	"context"
	"errors"
	"os/exec"
)

// FailureKind describes an operational failure, never the agent's business
// verdict. Unknown failures fail closed: text is not parsed to permit retries.
type FailureKind string

const (
	FailureUnknown        FailureKind = "unknown"
	FailureUnavailable    FailureKind = "unavailable"
	FailureQuota          FailureKind = "quota_exhausted"
	FailureAuthentication FailureKind = "authentication"
	FailureConfiguration  FailureKind = "configuration"
	FailurePolicy         FailureKind = "policy_denied"
	FailureSession        FailureKind = "session"
	FailureOutput         FailureKind = "invalid_output"
	FailureCanceled       FailureKind = "canceled"
	FailureDeadline       FailureKind = "deadline"
)

// OperationalError carries machine-readable evidence from an adapter. Only
// recognized structured protocol fields or local errors should assign a Kind.
// A Kind alone is NOT evidence that replaying the request is safe.
type OperationalError struct {
	Kind FailureKind
	Err  error
}

func (e *OperationalError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}
func (e *OperationalError) Unwrap() error { return e.Err }

// ClassifyFailure preserves cancellation precedence even for wrapped errors.
// Provider error prose, agent output and task verdicts are deliberately ignored.
func ClassifyFailure(err error) FailureKind {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureDeadline
	case errors.Is(err, context.Canceled), errors.Is(err, ErrInterruptedByQuiesce), errors.Is(err, ErrInterruptedByStop):
		return FailureCanceled
	}
	if operational, ok := err.(*OperationalError); ok && operational != nil {
		switch operational.Kind {
		case FailureUnavailable, FailureQuota, FailureAuthentication, FailureConfiguration, FailurePolicy, FailureSession, FailureOutput, FailureCanceled, FailureDeadline:
			inner := ClassifyFailure(operational.Err)
			var nested *OperationalError
			if inner == FailureUnknown && errors.As(operational.Err, &nested) {
				return FailureUnknown
			}
			if inner != "" && inner != FailureUnknown && inner != operational.Kind {
				return FailureUnknown // conflicting structured evidence fails closed
			}
			return operational.Kind
		}
		return FailureUnknown
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kind FailureKind
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			next := ClassifyFailure(child)
			if kind != "" && kind != next {
				return FailureUnknown
			}
			kind = next
		}
		if kind != "" {
			return kind
		}
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return ClassifyFailure(wrapped.Unwrap())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return FailureUnavailable
	}
	return FailureUnknown
}
