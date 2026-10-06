// Package platform assembles the trusted platform service: the runtime API,
// connection manager with ingress loops, outbox dispatcher and background
// scheduler (§3, §13).
package platform

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/services/api"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/ingress"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/outbox"
	"github.com/darcys22/steadmesh/services/scheduler"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
)

// Options configure a Platform.
type Options struct {
	Store     *store.Store
	Auth      auth.Authenticator
	Factories connections.Factories
	Secrets   connectors.Secrets
	Registry  *prometheus.Registry
	Log       *slog.Logger
	// Interval is the period of the scheduler and outbox loops.
	Interval time.Duration
	// RetryBackoff is the first delay before retrying a retryable connector call.
	RetryBackoff time.Duration
}

// Platform is a running platform service.
type Platform struct {
	Handler     http.Handler
	Connections *connections.Manager

	opts    Options
	metrics *metrics.Metrics
	wg      sync.WaitGroup
}

// New wires the platform. Background work starts with Start and stops when
// ctx is cancelled.
func New(ctx context.Context, o Options) *Platform {
	if o.Interval == 0 {
		o.Interval = 2 * time.Second
	}
	if o.RetryBackoff == 0 {
		o.RetryBackoff = 200 * time.Millisecond
	}
	m := metrics.New(o.Registry)
	sinks := func(orgID string) connectors.IngressSink {
		return &ingress.Sink{OrganizationID: orgID, Store: o.Store, Metrics: m, Log: o.Log}
	}
	conns := connections.New(ctx, o.Factories, o.Secrets, sinks, o.Log)
	gw := &gateway.Gateway{Ledger: o.Store, Trackers: conns, Metrics: m, Log: o.Log, Attempts: 3, Backoff: o.RetryBackoff}
	reg := tools.New(tools.Deps{Store: o.Store, Gateway: gw, Metrics: m, Log: o.Log})
	return &Platform{
		Handler: api.New(api.Config{Store: o.Store, Auth: o.Auth, Tools: reg, Connections: conns, Metrics: m,
			Gatherer: o.Registry, Log: o.Log}),
		Connections: conns,
		opts:        o,
		metrics:     m,
	}
}

// Start restores the adapters of every active organisation (so ingress
// resumes after a restart, A20) and starts the background loops.
func (p *Platform) Start(ctx context.Context) error {
	ids, err := p.opts.Store.ActiveOrganizations(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		org, err := p.opts.Store.Organization(ctx, id)
		if err != nil {
			return err
		}
		p.Connections.Sync(ctx, id, org.Manifest.Spec.Connections)
	}
	loop := &scheduler.Loop{Store: p.opts.Store, Metrics: p.metrics, Log: p.opts.Log, Interval: p.opts.Interval}
	disp := &outbox.Dispatcher{Store: p.opts.Store, Comms: p.Connections, Metrics: p.metrics, Log: p.opts.Log, Interval: p.opts.Interval}
	p.wg.Go(func() { loop.Run(ctx) })
	p.wg.Go(func() { disp.Run(ctx) })
	return nil
}

// Wait blocks until the background loops and ingress loops have stopped.
func (p *Platform) Wait() {
	p.wg.Wait()
	p.Connections.Close()
}
