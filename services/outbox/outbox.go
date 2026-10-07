// Package outbox delivers representatives' replies to humans through the
// communication adapter (§9.2). Each send is a connector operation recorded
// before the call; an ambiguous result is recorded as unknown and never
// blindly resent.
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

// Store is the persistence the dispatcher needs.
type Store interface {
	ClaimOutbox(ctx context.Context, limit int) ([]store.OutboxItem, error)
	ResolveOutbox(ctx context.Context, it store.OutboxItem, state, receipt, errText string, retryIn time.Duration) error
	ExpireSending(ctx context.Context) (int, error)
	Organization(ctx context.Context, orgID string) (*store.Organization, error)
}

// Comms resolves communication adapters.
type Comms interface {
	Communication(orgID, key string) (connectors.Communication, error)
}

// Refresher is implemented by Comms that can reload a rotated credential.
type Refresher interface {
	RefreshNow(ctx context.Context, orgID, key string) bool
}

// Dispatcher sends claimed outbox rows.
type Dispatcher struct {
	Store    Store
	Comms    Comms
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	Interval time.Duration
}

// Run dispatches until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	for {
		if _, err := d.Store.ExpireSending(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("expire interrupted sends", "error", err)
		}
		for {
			n, err := d.DispatchOnce(ctx)
			if err != nil && ctx.Err() == nil {
				d.Log.Error("outbox dispatch", "error", err)
			}
			if n == 0 || err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const batch = 20

// retryIn mirrors the inbox backoff: 1s, 8s, 64s, then 5m.
func retryIn(attempts int) time.Duration {
	d := time.Second
	for range attempts - 1 {
		d *= 8
	}
	return min(d, 5*time.Minute)
}

// DispatchOnce sends one batch and returns how many rows it handled.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	items, err := d.Store.ClaimOutbox(ctx, batch)
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		state, receipt, errText := d.send(ctx, it)
		wait := time.Duration(0)
		if state == store.OutboxPending {
			if it.Attempts >= store.MaxAttempts {
				state = store.OutboxDead
			} else {
				wait = retryIn(it.Attempts)
			}
		}
		log := d.Log.With("organization_id", it.OrganizationID, "message_id", it.MessageID, "operation_id", it.OperationID,
			"connection", it.Connection, "attempt", it.Attempts)
		if err := d.Store.ResolveOutbox(context.WithoutCancel(ctx), it, state, receipt, errText, wait); err != nil {
			return 0, err
		}
		d.Metrics.ConnectorOps.WithLabelValues(it.Connection, state).Inc()
		switch state {
		case store.OutboxSent:
			log.Info("outbound message delivered", "receipt", receipt)
		case store.OutboxUnknown:
			d.Metrics.ConnectorUnknown.WithLabelValues(it.Connection).Inc()
			log.Warn("outbound message outcome unknown; not resending", "error", errText)
		case store.OutboxPending:
			d.Metrics.ConnectorRetries.WithLabelValues(it.Connection).Inc()
			log.Warn("outbound message will be retried", "error", errText, "retry_in", wait)
		default:
			log.Error("outbound message failed", "error", errText)
		}
	}
	return len(items), nil
}

func (d *Dispatcher) send(ctx context.Context, it store.OutboxItem) (state, receipt, errText string) {
	org, err := d.Store.Organization(ctx, it.OrganizationID)
	if err != nil {
		return store.OutboxDead, "", "organisation unavailable: " + err.Error()
	}
	b, ok := org.Manifest.Spec.ChannelBindings[it.Binding]
	if !ok || b.Connection != it.Connection {
		return store.OutboxDead, "", "channel binding " + it.Binding + " no longer exists"
	}
	comm, err := d.Comms.Communication(it.OrganizationID, it.Connection)
	if err != nil {
		// Nothing was sent, so retrying is safe.
		return store.OutboxPending, "", err.Error()
	}
	channel, thread := store.SplitExternalRef(it.ExternalRef)
	receipt, err = comm.Send(ctx, connectors.OutboundMessage{
		ExternalUserID: b.ExternalUserID, ChannelID: channel, ThreadRef: thread, Text: it.Body, IdempotencyKey: it.OperationID,
	})
	switch {
	case err == nil:
		return store.OutboxSent, receipt, ""
	case errors.Is(err, connectors.ErrRetryable):
		return store.OutboxPending, "", err.Error()
	case errors.Is(err, connectors.ErrUnauthorized):
		// Rejected before sending. The credential may have been rotated:
		// reload it and retry under the normal bounded backoff rather than
		// dropping the reply.
		if r, ok := d.Comms.(Refresher); ok {
			r.RefreshNow(ctx, it.OrganizationID, it.Connection)
		}
		return store.OutboxPending, "", err.Error()
	case errors.Is(err, connectors.ErrPermanent):
		return store.OutboxDead, "", err.Error()
	default:
		return store.OutboxUnknown, "", err.Error()
	}
}
