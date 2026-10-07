// Package gateway is the capability gateway and operation ledger for
// work-tracker connections (§10.1, §10.3, contracts.md "Gateway and operation
// ledger"). An operation id is recorded before any side effect, duplicate
// idempotency keys return the recorded operation instead of re-executing, and
// a lost response is resolved by read-back or recorded as unknown, never
// blindly replayed (A10).
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

var (
	ErrForbidden = errors.New("operation not granted")
	ErrInvalid   = errors.New("invalid request")
)

// Ledger is the durable operation record.
type Ledger interface {
	BeginOperation(ctx context.Context, f store.Fence, in store.NewOperation) (*runtimeapi.Operation, bool, error)
	FinishOperation(ctx context.Context, id, status, receipt string, result json.RawMessage, errText string, attempts int) error
}

// Trackers resolves a connection's tracker adapter.
type Trackers interface {
	Tracker(orgID, key string) (connectors.Tracker, error)
}

// Refresher is implemented by Trackers that can reload a rotated credential.
// RefreshNow reports whether a different credential is now in use.
type Refresher interface {
	RefreshNow(ctx context.Context, orgID, key string) bool
}

// Gateway invokes tracker operations on behalf of seats.
type Gateway struct {
	Ledger   Ledger
	Trackers Trackers
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	// Attempts bounds tries of a retryable failure; Backoff is the first delay.
	Attempts int
	Backoff  time.Duration
}

// Request is one connections.invoke call.
type Request struct {
	Seat           *store.Seat
	Generation     int64
	ExecutionID    string
	Connection     string
	Operation      string
	Params         json.RawMessage
	IdempotencyKey string
}

// targetKeys are the parameters that name an operation's target, in order of
// precedence. Grants with targets restrict calls to matching values.
var targetKeys = []string{"target", "project_id", "team_id", "issue_id"}

func target(params map[string]any) string {
	for _, k := range targetKeys {
		if v, ok := params[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Invoke runs the gateway algorithm and returns the recorded operation.
func (g *Gateway) Invoke(ctx context.Context, req Request) (*runtimeapi.Operation, error) {
	if len(req.Params) == 0 {
		req.Params = json.RawMessage(`{}`)
	}
	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("%w: params must be a JSON object", ErrInvalid)
	}
	canonical, _ := json.Marshal(params)
	tgt := target(params)
	if !policy.ConnectionAllowed(&req.Seat.Manifest, req.Connection, req.Operation, tgt) {
		return nil, fmt.Errorf("%w: %s on connection %s", ErrForbidden, req.Operation, req.Connection)
	}
	tracker, err := g.Trackers.Tracker(req.Seat.OrganizationID, req.Connection)
	if err != nil {
		return nil, err
	}
	key := req.IdempotencyKey
	switch {
	case key != "":
	case tracker.ReadOnly(req.Operation):
		// Reads have no effect to deduplicate; each call reads afresh.
		key = "read:" + uuid.NewString()
	default:
		key = digest(req.Seat.ID, req.Operation, string(canonical))
	}
	op, created, err := g.Ledger.BeginOperation(ctx, store.Fence{SeatID: req.Seat.ID, Generation: req.Generation}, store.NewOperation{
		OrganizationID: req.Seat.OrganizationID, SeatID: req.Seat.ID, ExecutionID: req.ExecutionID,
		Connection: req.Connection, Operation: req.Operation, Target: tgt,
		RequestHash: digest(req.Connection, req.Operation, string(canonical)), IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}
	log := g.Log.With("organization_id", req.Seat.OrganizationID, "seat_id", req.Seat.ID, "execution_id", req.ExecutionID,
		"operation_id", op.ID, "connection", req.Connection, "operation", req.Operation)
	if !created {
		log.Info("idempotency key reused; returning recorded operation", "status", op.Status)
		return op, nil
	}
	status, res, errText, attempts := g.execute(ctx, tracker, req, canonical, op.ID, log)
	op.Status, op.Error = status, errText
	if res != nil {
		op.ExternalReceipt, op.Result = res.Receipt, res.Data
	}
	// The outcome describes the external world, so it is recorded even if the
	// caller's context is gone.
	if err := g.Ledger.FinishOperation(context.WithoutCancel(ctx), op.ID, status, op.ExternalReceipt, op.Result, errText, attempts); err != nil {
		return nil, err
	}
	g.Metrics.ConnectorOps.WithLabelValues(req.Connection, status).Inc()
	if status == store.OpUnknown {
		g.Metrics.ConnectorUnknown.WithLabelValues(req.Connection).Inc()
	}
	log.Info("connector operation finished", "status", status, "attempts", attempts)
	return op, nil
}

func (g *Gateway) execute(ctx context.Context, tracker connectors.Tracker, req Request, params []byte, opID string, log *slog.Logger) (string, *connectors.Result, string, int) {
	backoff := g.Backoff
	readOnly := tracker.ReadOnly(req.Operation)
	refreshed := false
	for attempt := 1; ; attempt++ {
		res, err := tracker.Invoke(ctx, req.Operation, params, opID)
		retryable := errors.Is(err, connectors.ErrRetryable) || (readOnly && err != nil && !isPermanent(err))
		switch {
		case err == nil:
			return store.OpSucceeded, &res, "", attempt
		case errors.Is(err, connectors.ErrUnauthorized) && !refreshed:
			// A rejected credential means nothing was applied. If the secret
			// was rotated, retry once with the new credential.
			refreshed = true
			if next := g.refreshed(ctx, req); next != nil {
				log.Info("retrying with a refreshed credential", "attempt", attempt)
				tracker = next
				continue
			}
			return store.OpFailed, nil, err.Error(), attempt
		case isPermanent(err):
			return store.OpFailed, nil, err.Error(), attempt
		case retryable && attempt < g.Attempts:
			g.Metrics.ConnectorRetries.WithLabelValues(req.Connection).Inc()
			log.Warn("retrying connector operation", "attempt", attempt, "error", err)
			select {
			case <-ctx.Done():
				return store.OpFailed, nil, "cancelled before retry: " + err.Error(), attempt
			case <-time.After(backoff):
			}
			backoff *= 2
		case retryable:
			// Retryable means the request was definitely not applied.
			return store.OpFailed, nil, err.Error(), attempt
		default:
			// Ambiguous, or an error the adapter did not classify: the effect
			// may have happened. Read back before deciding.
			found, ferr := tracker.FindByOperation(context.WithoutCancel(ctx), req.Operation, params, opID)
			if ferr == nil && found != nil {
				log.Info("ambiguous outcome resolved by read-back")
				return store.OpSucceeded, found, "", attempt
			}
			detail := "outcome unknown: " + err.Error()
			if ferr != nil {
				detail += "; read-back failed: " + ferr.Error()
			}
			log.Warn("connector operation outcome unknown; it will not be retried automatically", "error", err)
			return store.OpUnknown, nil, detail, attempt
		}
	}
}

// refreshed reloads the connection's credential and returns the new tracker,
// or nil when the credential did not change.
func (g *Gateway) refreshed(ctx context.Context, req Request) connectors.Tracker {
	r, ok := g.Trackers.(Refresher)
	if !ok || !r.RefreshNow(ctx, req.Seat.OrganizationID, req.Connection) {
		return nil
	}
	t, err := g.Trackers.Tracker(req.Seat.OrganizationID, req.Connection)
	if err != nil {
		return nil
	}
	return t
}

func isPermanent(err error) bool {
	return errors.Is(err, connectors.ErrPermanent) || errors.Is(err, connectors.ErrUnauthorized)
}
