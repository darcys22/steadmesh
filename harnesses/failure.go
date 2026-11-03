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
	case errors.Is(err, exec.ErrNotFound):
		return FailureUnavailable
	}
	var operational *OperationalError
	if errors.As(err, &operational) && operational != nil {
		switch operational.Kind {
		case FailureUnavailable, FailureQuota, FailureAuthentication, FailureConfiguration, FailurePolicy, FailureSession, FailureOutput, FailureCanceled, FailureDeadline:
			return operational.Kind
		}
	}
	return FailureUnknown
}
