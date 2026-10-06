// Package ingress accepts verified inbound human events (§9.1, §9.2).
// Authorisation comes only from the adapter-verified account and user ids,
// matched against the declared channel bindings; message text is never
// consulted (A05). An event is acknowledged only after it is committed, and
// replays are deduplicated by the store (A09).
package ingress

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

// Store is the persistence the sink needs.
type Store interface {
	Organization(ctx context.Context, orgID string) (*store.Organization, error)
	SeatByKey(ctx context.Context, orgID, key string) (*store.Seat, error)
	IngestHuman(ctx context.Context, in store.HumanMessage) (*store.Ingested, error)
}

// Sink is the connectors.IngressSink of one organisation.
type Sink struct {
	OrganizationID string
	Store          Store
	Metrics        *metrics.Metrics
	Log            *slog.Logger
}

var _ connectors.IngressSink = (*Sink)(nil)

// Accept implements connectors.IngressSink. Events that no binding authorises
// are dropped (nil is returned so the adapter does not redeliver them).
func (s *Sink) Accept(ctx context.Context, connection string, ev connectors.InboundEvent) error {
	org, err := s.Store.Organization(ctx, s.OrganizationID)
	if err != nil {
		return fmt.Errorf("load organisation: %w", err)
	}
	log := s.Log.With("organization_id", s.OrganizationID, "connection", connection, "event_id", ev.EventID)
	drop := func(reason string) error {
		s.Metrics.IngressEvents.WithLabelValues(connection, reason).Inc()
		log.Warn("inbound event dropped", "reason", reason, "account_id", ev.AccountID, "user_id", ev.UserID)
		return nil
	}
	decl, ok := org.Manifest.Spec.Connections[connection]
	if !ok {
		return drop("unknown_connection")
	}
	if decl.AccountID != "" && ev.AccountID != decl.AccountID {
		return drop("unverified_account")
	}
	bindingKey, binding, ok := findBinding(org.Manifest.Spec.ChannelBindings, connection, ev.UserID)
	if !ok {
		return drop("unknown_user")
	}
	eventID := ev.EventID
	if eventID == "" {
		eventID = "ts:" + ev.ChannelID + ":" + ev.MessageTS
	}
	rep, err := s.Store.SeatByKey(ctx, s.OrganizationID, binding.Seat)
	if err != nil {
		return fmt.Errorf("representative %s: %w", binding.Seat, err)
	}
	res, err := s.Store.IngestHuman(ctx, store.HumanMessage{
		OrganizationID: s.OrganizationID, Connection: connection, Binding: bindingKey, ExternalUserID: ev.UserID,
		ExternalRef: store.ExternalRef(ev.ChannelID, ev.ThreadRef), EventID: eventID, RepresentativeID: rep.ID, Body: ev.Text,
	})
	if err != nil {
		s.Metrics.IngressEvents.WithLabelValues(connection, "error").Inc()
		return fmt.Errorf("persist inbound event: %w", err)
	}
	log = log.With("message_id", res.MessageID, "seat_id", rep.ID, "binding", bindingKey)
	switch {
	case res.Duplicate:
		s.Metrics.IngressEvents.WithLabelValues(connection, "duplicate").Inc()
		log.Info("duplicate inbound event acknowledged")
		return nil
	case res.OverCap:
		s.Metrics.IngressEvents.WithLabelValues(connection, "over_cap").Inc()
		log.Warn("representative inbox over capacity; message accepted anyway", "cap", store.MaxPendingDeliveries)
	}
	s.Metrics.IngressEvents.WithLabelValues(connection, "accepted").Inc()
	s.Metrics.MessagesAccepted.WithLabelValues(store.OriginHuman).Inc()
	log.Info("inbound message accepted")
	return nil
}

func findBinding(bindings map[string]spec.ChannelBinding, connection, user string) (string, spec.ChannelBinding, bool) {
	if user == "" {
		return "", spec.ChannelBinding{}, false
	}
	for k, b := range bindings {
		if b.Connection == connection && b.ExternalUserID == user {
			return k, b, true
		}
	}
	return "", spec.ChannelBinding{}, false
}
