// Package connections builds and supervises connector adapters for every
// declared connection (§10). Adapters are constructed from injected
// factories, so this package never depends on concrete connectors; external
// credentials are resolved here and stay in the platform process.
package connections

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// Factories construct adapters by adapter name.
type Factories struct {
	Communication map[string]func(connectors.Config) (connectors.Communication, error)
	Tracker       map[string]func(connectors.Config) (connectors.Tracker, error)
	Model         map[string]func(connectors.Config) (connectors.Model, error)
}

// SinkFactory returns the ingress sink for an organisation's communication
// connections.
type SinkFactory func(orgID string) connectors.IngressSink

// ErrNotConfigured means the connection is not declared or its adapter could
// not be built; the wrapped error says why.
var ErrNotConfigured = errors.New("connection not available")

// Manager owns the adapters of all organisations.
type Manager struct {
	factories Factories
	secrets   connectors.Secrets
	sinks     SinkFactory
	http      *http.Client
	log       *slog.Logger
	base      context.Context

	// syncMu serialises rebuilds so an adapter's ingress loop starts once.
	syncMu sync.Mutex
	mu     sync.Mutex
	orgs   map[string]map[string]*conn
	wg     sync.WaitGroup
}

type conn struct {
	fingerprint string
	comm        connectors.Communication
	tracker     connectors.Tracker
	model       connectors.Model
	err         error
	stop        context.CancelFunc
}

// New returns a Manager. Ingress loops run until ctx is cancelled.
func New(ctx context.Context, f Factories, secrets connectors.Secrets, sinks SinkFactory, log *slog.Logger) *Manager {
	return &Manager{
		factories: f, secrets: secrets, sinks: sinks, log: log, base: ctx,
		http: &http.Client{Timeout: 60 * time.Second},
		orgs: map[string]map[string]*conn{},
	}
}

func fingerprint(c spec.Connection) string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return string(sum[:])
}

// Sync (re)builds the organisation's adapters so they match its declared
// connections. Unchanged, healthy adapters are kept; failed builds are retried.
// Build failures are recorded and reported by Verify.
func (m *Manager) Sync(ctx context.Context, orgID string, declared map[string]spec.Connection) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	current := m.orgs[orgID]
	m.mu.Unlock()

	next := map[string]*conn{}
	var stale []*conn
	for key, c := range declared {
		fp := fingerprint(c)
		if cur, ok := current[key]; ok && cur.fingerprint == fp && cur.err == nil {
			next[key] = cur
			continue
		}
		if cur, ok := current[key]; ok {
			stale = append(stale, cur)
		}
		next[key] = m.build(ctx, orgID, key, c, fp)
	}
	for key, cur := range current {
		if _, ok := declared[key]; !ok {
			stale = append(stale, cur)
		}
	}
	for _, c := range stale {
		if c.stop != nil {
			c.stop()
		}
	}
	m.mu.Lock()
	m.orgs[orgID] = next
	m.mu.Unlock()
}

func (m *Manager) build(ctx context.Context, orgID, key string, c spec.Connection, fp string) *conn {
	out := &conn{fingerprint: fp}
	cfg := connectors.Config{OrganizationID: orgID, Key: key, Adapter: c.Adapter, AccountID: c.AccountID,
		Endpoint: c.EndpointRef, Extra: c.Config, HTTP: m.http}
	if c.SecretRef != "" {
		secret, err := m.secrets.Resolve(ctx, c.SecretRef)
		if err != nil {
			out.err = fmt.Errorf("resolve secret for connection %s: %w", key, err)
			return out
		}
		cfg.Secret = secret
	}
	switch {
	case m.factories.Communication[c.Adapter] != nil:
		out.comm, out.err = m.factories.Communication[c.Adapter](cfg)
		if out.err == nil {
			m.runIngress(orgID, key, out)
		}
	case m.factories.Tracker[c.Adapter] != nil:
		out.tracker, out.err = m.factories.Tracker[c.Adapter](cfg)
	case m.factories.Model[c.Adapter] != nil:
		out.model, out.err = m.factories.Model[c.Adapter](cfg)
	default:
		out.err = fmt.Errorf("no adapter %q is installed", c.Adapter)
	}
	if out.err != nil {
		m.log.Warn("connection unavailable", "organization_id", orgID, "connection", key, "error", out.err)
	}
	return out
}

// runIngress keeps the adapter's Run loop going with capped backoff (§9.2).
func (m *Manager) runIngress(orgID, key string, c *conn) {
	ctx, cancel := context.WithCancel(m.base)
	c.stop = cancel
	sink := m.sinks(orgID)
	m.wg.Go(func() {
		backoff := time.Second
		for {
			start := time.Now()
			err := c.comm.Run(ctx, sink)
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("ingress loop exited", "organization_id", orgID, "connection", key, "error", err)
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 30*time.Second)
		}
	})
}

// Remove stops and forgets the organisation's adapters.
func (m *Manager) Remove(orgID string) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	conns := m.orgs[orgID]
	delete(m.orgs, orgID)
	m.mu.Unlock()
	for _, c := range conns {
		if c.stop != nil {
			c.stop()
		}
	}
}

// Close stops every ingress loop and waits for them.
func (m *Manager) Close() {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	orgs := m.orgs
	m.orgs = map[string]map[string]*conn{}
	m.mu.Unlock()
	for _, conns := range orgs {
		for _, c := range conns {
			if c.stop != nil {
				c.stop()
			}
		}
	}
	m.wg.Wait()
}

func (m *Manager) get(orgID, key string) (*conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.orgs[orgID][key]
	if !ok {
		return nil, fmt.Errorf("%w: %s is not declared", ErrNotConfigured, key)
	}
	if c.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotConfigured, c.err)
	}
	return c, nil
}

// Tracker returns the work-tracker adapter of a connection.
func (m *Manager) Tracker(orgID, key string) (connectors.Tracker, error) {
	c, err := m.get(orgID, key)
	if err != nil {
		return nil, err
	}
	if c.tracker == nil {
		return nil, fmt.Errorf("%w: %s is not a work tracker", ErrNotConfigured, key)
	}
	return c.tracker, nil
}

// Communication returns the communication adapter of a connection.
func (m *Manager) Communication(orgID, key string) (connectors.Communication, error) {
	c, err := m.get(orgID, key)
	if err != nil {
		return nil, err
	}
	if c.comm == nil {
		return nil, fmt.Errorf("%w: %s is not a communication connection", ErrNotConfigured, key)
	}
	return c.comm, nil
}

// Model returns the model adapter of a connection.
func (m *Manager) Model(orgID, key string) (connectors.Model, error) {
	c, err := m.get(orgID, key)
	if err != nil {
		return nil, err
	}
	if c.model == nil {
		return nil, fmt.Errorf("%w: %s is not a model connection", ErrNotConfigured, key)
	}
	return c.model, nil
}

const checkTimeout = 10 * time.Second

// Verify re-checks the organisation's connections, ingress and bindings
// without creating business work or sending messages (§5.4).
func (m *Manager) Verify(ctx context.Context, orgID string, manifest *compile.Manifest) runtimeapi.VerifyResponse {
	m.Sync(ctx, orgID, manifest.Spec.Connections)
	out := runtimeapi.VerifyResponse{
		Connections: map[string]runtimeapi.CheckResult{},
		Ingress:     map[string]runtimeapi.CheckResult{},
		Bindings:    map[string]runtimeapi.CheckResult{},
	}
	check := func(fn func(context.Context) error) runtimeapi.CheckResult {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		if err := fn(cctx); err != nil {
			return runtimeapi.CheckResult{Detail: err.Error()}
		}
		return runtimeapi.CheckResult{OK: true}
	}
	keys := make([]string, 0, len(manifest.Spec.Connections))
	for k := range manifest.Spec.Connections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c, err := m.get(orgID, key)
		if err != nil {
			out.Connections[key] = runtimeapi.CheckResult{Detail: err.Error()}
			if m.factories.Communication[manifest.Spec.Connections[key].Adapter] != nil {
				out.Ingress[key] = runtimeapi.CheckResult{Detail: "connection unavailable"}
			}
			continue
		}
		switch {
		case c.comm != nil:
			out.Connections[key] = check(c.comm.Verify)
			ok, detail := c.comm.Healthy()
			out.Ingress[key] = runtimeapi.CheckResult{OK: ok, Detail: detail}
		case c.tracker != nil:
			out.Connections[key] = check(c.tracker.Verify)
		case c.model != nil:
			out.Connections[key] = check(c.model.Verify)
		}
	}
	for key, b := range manifest.Spec.ChannelBindings {
		if !out.Connections[b.Connection].OK {
			out.Bindings[key] = runtimeapi.CheckResult{Detail: "connection " + b.Connection + " is not authenticated"}
			continue
		}
		comm, err := m.Communication(orgID, b.Connection)
		if err != nil {
			out.Bindings[key] = runtimeapi.CheckResult{Detail: err.Error()}
			continue
		}
		out.Bindings[key] = check(func(ctx context.Context) error { return comm.VerifyUser(ctx, b.ExternalUserID) })
	}
	return out
}
