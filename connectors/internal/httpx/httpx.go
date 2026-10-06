// Package httpx holds HTTP helpers shared by connector adapters: a transport
// that records whether a request reached the wire, so that a failure can be
// classified as "definitely not applied" or "outcome unknown" (§10.3).
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

// DefaultTimeout bounds a single adapter request when the caller supplies no client.
const DefaultTimeout = 30 * time.Second

// SentError wraps a transport error that happened after the request was fully
// written. The server may have applied it.
type SentError struct{ Err error }

func (e *SentError) Error() string { return "after request was sent: " + e.Err.Error() }
func (e *SentError) Unwrap() error { return e.Err }

type trackingTransport struct{ base http.RoundTripper }

func (t trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.base.RoundTrip(req)
	if err != nil && wrote.Load() {
		return nil, &SentError{Err: err}
	}
	return resp, err
}

// Client returns a copy of c (or a default client) whose transport marks
// post-write failures with SentError.
func Client(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	} else {
		out.Timeout = DefaultTimeout
	}
	base := out.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(trackingTransport); !ok {
		out.Transport = trackingTransport{base: base}
	}
	return &out
}

// ClassifyTransport maps a transport-level error (no HTTP response) to a
// connector outcome. Errors after the request was written are ambiguous;
// errors before it (dial failures, DNS, cancelled before send) are retryable.
func ClassifyTransport(err error) error {
	if err == nil {
		return nil
	}
	var sent *SentError
	if errors.As(err, &sent) {
		return fmt.Errorf("%w: %v", connectors.ErrAmbiguous, err)
	}
	return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
}

// ClassifyStatus maps a non-2xx HTTP status to a connector outcome.
func ClassifyStatus(code int, detail string) error {
	switch {
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: http %d %s", connectors.ErrRetryable, code, detail)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: http %d %s", connectors.ErrUnauthorized, code, detail)
	case code >= 500:
		return fmt.Errorf("%w: http %d %s", connectors.ErrAmbiguous, code, detail)
	default:
		return fmt.Errorf("%w: http %d %s", connectors.ErrPermanent, code, detail)
	}
}
