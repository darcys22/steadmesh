// Package connections builds and supervises connector adapters for every
// declared connection (§10). Adapters are constructed from injected
// factories, so this package never depends on concrete connectors; external
// credentials are resolved here and stay in the platform process.
//
// Credentials are refreshed independently of configuration (refresh.go): a
// changed secret is validated before it replaces the credential in use, and
// a replacement that fails validation is never activated.
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

// ErrNotConfigured means the connection is not declared or has no usable
// adapter; the wrapped error says why.
var ErrNotConfigured = errors.New("connection not available")

// Options configure a Manager. Zero durations take the defaults.
type Options struct {
	Factories Factories
	Secrets   connectors.Secrets
	Sinks     SinkFactory
	Log       *slog.Logger
	// RefreshInterval is how often every credential is re-resolved: the
	// Vault poll, and the backstop for missed Kubernetes watch events.
	RefreshInterval time.Duration
	// Grace is how long the previous credential stays in use after its
	// replacement failed validation or its secret was deleted.
	Grace time.Duration
	// MaxStale is how long the current credential stays in use while its
	// secret cannot be read or a replacement cannot be validated.
	MaxStale time.Duration
	// HealthyWait bounds how long a replacement ingress connection may take
	// to come up before the previous one is stopped.
	HealthyWait time.Duration
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Defaults for Options.
const (
	DefaultRefreshInterval = 60 * time.Second
	DefaultGrace           = 10 * time.Minute
	DefaultMaxStale        = 24 * time.Hour
	DefaultHealthyWait     = 30 * time.Second
	// ForcedRefreshInterval rate-limits refreshes triggered by
	// authentication failures, per connection.
	ForcedRefreshInterval = 30 * time.Second
	watchDebounce         = 2 * time.Second
	checkTimeout          = 10 * time.Second
)

// Manager owns the adapters of all organisations.
type Manager struct {
	opts   Options
	http   *http.Client
	log    *slog.Logger
	base   context.Context
	cancel context.CancelFunc
	now    func() time.Time

	// syncMu serialises Sync, Remove and Close.
	syncMu sync.Mutex
	// mu guards orgs and every conn's adapter and status fields.
	mu   sync.Mutex
	orgs map[string]map[string]*conn
	wg   sync.WaitGroup
}

// adapters is one built set of adapters for a connection.
type adapters struct {
	comm    connectors.Communication
	tracker connectors.Tracker
	model   connectors.Model
}

func (a adapters) verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	switch {
	case a.comm != nil:
		return a.comm.Verify(ctx)
	case a.tracker != nil:
		return a.tracker.Verify(ctx)
	case a.model != nil:
		return a.model.Verify(ctx)
	}
	return errors.New("no adapter")
}

// ingress is a running communication Run loop owned by one connection.
type ingress struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (i *ingress) stop() {
	if i != nil {
		i.cancel()
		<-i.done
	}
}

type conn struct {
	org, key    string
	decl        spec.Connection
	uses        []connectors.ModelUse
	fingerprint string

	// refreshMu serialises credential refreshes of this connection.
	refreshMu  sync.Mutex
	lastForced time.Time // guarded by refreshMu

	// Guarded by Manager.mu.
	adapters
	ingress *ingress
	// err says why the connection is unavailable; nil when usable.
	err error
	// digest identifies the secret content in use.
	digest string
	// rejected is the digest of a replacement that failed validation.
	rejected     string
	cred         runtimeapi.CredentialStatus
	failingSince time.Time
	prevUntil    time.Time
}

func (c *conn) usable() bool {
	return c.err == nil && (c.comm != nil || c.tracker != nil || c.model != nil)
}

// New returns a Manager. Ingress, refresh and watch loops run until ctx is
// cancelled.
func New(ctx context.Context, o Options) *Manager {
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = DefaultRefreshInterval
	}
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.MaxStale <= 0 {
		o.MaxStale = DefaultMaxStale
	}
	if o.HealthyWait <= 0 {
		o.HealthyWait = DefaultHealthyWait
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	base, cancel := context.WithCancel(ctx)
	m := &Manager{
		opts: o, log: o.Log, base: base, cancel: cancel, now: o.Now,
		http: &http.Client{Timeout: 60 * time.Second},
		orgs: map[string]map[string]*conn{},
	}
	m.startRefresh()
	return m
}

func fingerprint(c spec.Connection, uses []connectors.ModelUse) string {
	b, _ := json.Marshal(struct {
		C spec.Connection
		U []connectors.ModelUse
	}{c, uses})
	sum := sha256.Sum256(b)
	return string(sum[:])
}

// SyncManifest syncs the organisation's connections from its compiled
// manifest, including the models and APIs its seats use, which model
// connections verify.
func (m *Manager) SyncManifest(ctx context.Context, orgID string, manifest *compile.Manifest) {
	uses := map[string][]connectors.ModelUse{}
	for conn, us := range compile.ModelUses(manifest) {
		for _, u := range us {
			uses[conn] = append(uses[conn], connectors.ModelUse{ID: u.ID, API: u.API})
		}
	}
	m.sync(ctx, orgID, manifest.Spec.Connections, uses)
}

// Sync (re)builds the organisation's adapters so they match its declared
// connections. A changed declaration is rebuilt; an unchanged one is kept
// and, if it is unavailable, retried through the same validated refresh
// that handles rotation, so a credential known to be invalid is never
// reactivated.
func (m *Manager) Sync(ctx context.Context, orgID string, declared map[string]spec.Connection) {
	m.sync(ctx, orgID, declared, nil)
}

func (m *Manager) sync(ctx context.Context, orgID string, declared map[string]spec.Connection, uses map[string][]connectors.ModelUse) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	current := m.orgs[orgID]
	m.mu.Unlock()

	next := map[string]*conn{}
	var stale, retry []*conn
	for key, c := range declared {
		fp := fingerprint(c, uses[key])
		if cur, ok := current[key]; ok && cur.fingerprint == fp {
			next[key] = cur
			m.mu.Lock()
			usable := cur.usable()
			m.mu.Unlock()
			if !usable {
				retry = append(retry, cur)
			}
			continue
		}
		if cur, ok := current[key]; ok {
			stale = append(stale, cur)
		}
		next[key] = m.build(ctx, orgID, key, c, uses[key], fp)
	}
	for key, cur := range current {
		if _, ok := declared[key]; !ok {
			stale = append(stale, cur)
		}
	}
	m.mu.Lock()
	m.orgs[orgID] = next
	m.mu.Unlock()
	for _, c := range stale {
		m.retire(c)
	}
	for _, c := range retry {
		m.refresh(ctx, c)
	}
}

// build constructs a connection from its declaration without validating it
// first: a new or changed declaration is the operator's intent, and Verify
// reports whether it works.
func (m *Manager) build(ctx context.Context, orgID, key string, d spec.Connection, uses []connectors.ModelUse, fp string) *conn {
	c := &conn{org: orgID, key: key, decl: d, uses: uses, fingerprint: fp}
	values, res, err := m.resolve(ctx, d)
	if err != nil {
		c.err = fmt.Errorf("resolve secret for connection %s: %w", key, err)
		c.cred = runtimeapi.CredentialStatus{State: runtimeapi.CredentialUnavailable, Error: c.err.Error()}
		m.log.Warn("connection unavailable", "organization_id", orgID, "connection", key, "error", c.err)
		return c
	}
	a, err := m.construct(c, values)
	if err != nil {
		c.err = err
		c.cred = runtimeapi.CredentialStatus{State: runtimeapi.CredentialUnavailable, Error: err.Error()}
		m.log.Warn("connection unavailable", "organization_id", orgID, "connection", key, "error", err)
		return c
	}
	c.adapters = a
	c.digest = res.Digest
	c.cred = m.currentStatus(d, res)
	if a.comm != nil {
		c.ingress = m.runIngress(orgID, key, a.comm)
	}
	return c
}

func (m *Manager) resolve(ctx context.Context, d spec.Connection) (map[string]string, connectors.Resolved, error) {
	if d.SecretRef == "" {
		return nil, connectors.Resolved{}, nil
	}
	res, err := m.opts.Secrets.Resolve(ctx, d.SecretRef)
	if err != nil {
		return nil, res, err
	}
	return res.Values, res, nil
}

func (m *Manager) currentStatus(d spec.Connection, res connectors.Resolved) runtimeapi.CredentialStatus {
	if d.SecretRef == "" {
		return runtimeapi.CredentialStatus{State: runtimeapi.CredentialNone}
	}
	now := m.now()
	return runtimeapi.CredentialStatus{State: runtimeapi.CredentialCurrent, SecretVersion: res.Version, RefreshedAt: &now}
}

// construct builds adapters without starting ingress.
func (m *Manager) construct(c *conn, secret map[string]string) (adapters, error) {
	d := c.decl
	cfg := connectors.Config{OrganizationID: c.org, Key: c.key, Adapter: d.Adapter, AccountID: d.AccountID,
		Endpoint: d.EndpointRef, Extra: d.Config, Model: d.Model, ModelUses: c.uses, HTTP: m.http, Secret: secret}
	var a adapters
	var err error
	switch f := m.opts.Factories; {
	case f.Communication[d.Adapter] != nil:
		a.comm, err = f.Communication[d.Adapter](cfg)
	case f.Tracker[d.Adapter] != nil:
		a.tracker, err = f.Tracker[d.Adapter](cfg)
	case f.Model[d.Adapter] != nil:
		a.model, err = f.Model[d.Adapter](cfg)
	default:
		err = fmt.Errorf("no adapter %q is installed", d.Adapter)
	}
	return a, err
}

// runIngress keeps the adapter's Run loop going with capped backoff (§9.2).
// The returned handle stops it and waits for it to exit.
func (m *Manager) runIngress(orgID, key string, comm connectors.Communication) *ingress {
	ctx, cancel := context.WithCancel(m.base)
	in := &ingress{cancel: cancel, done: make(chan struct{})}
	sink := m.opts.Sinks(orgID)
	m.wg.Go(func() {
		defer close(in.done)
		backoff := time.Second
		for {
			start := time.Now()
			err := comm.Run(ctx, sink)
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
	return in
}

// retire stops a connection that is no longer declared or was replaced.
func (m *Manager) retire(c *conn) {
	m.mu.Lock()
	in := c.ingress
	c.ingress = nil
	m.mu.Unlock()
	in.stop()
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
		m.retire(c)
	}
}

// Close stops every ingress, refresh and watch loop and waits for them.
func (m *Manager) Close() {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.cancel()
	m.mu.Lock()
	orgs := m.orgs
	m.orgs = map[string]map[string]*conn{}
	m.mu.Unlock()
	for _, conns := range orgs {
		for _, c := range conns {
			m.retire(c)
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.model == nil {
		return nil, fmt.Errorf("%w: %s is not a model connection", ErrNotConfigured, key)
	}
	return c.model, nil
}

// Credentials reports the credential refresh state of the organisation's
// connections.
func (m *Manager) Credentials(orgID string) map[string]runtimeapi.CredentialStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]runtimeapi.CredentialStatus{}
	for k, c := range m.orgs[orgID] {
		out[k] = c.cred
	}
	return out
}

// Verify re-checks the organisation's connections, ingress and bindings
// without creating business work or sending messages (§5.4).
func (m *Manager) Verify(ctx context.Context, orgID string, manifest *compile.Manifest) runtimeapi.VerifyResponse {
	m.SyncManifest(ctx, orgID, manifest)
	// A requested verification sees rotated secrets now, not at the next
	// refresh tick.
	m.refreshOrg(ctx, orgID)
	out := runtimeapi.VerifyResponse{
		Connections: map[string]runtimeapi.CheckResult{},
		Ingress:     map[string]runtimeapi.CheckResult{},
		Bindings:    map[string]runtimeapi.CheckResult{},
		Credentials: m.Credentials(orgID),
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
			if m.opts.Factories.Communication[manifest.Spec.Connections[key].Adapter] != nil {
				out.Ingress[key] = runtimeapi.CheckResult{Detail: "connection unavailable"}
			}
			continue
		}
		m.mu.Lock()
		a := c.adapters
		m.mu.Unlock()
		switch {
		case a.comm != nil:
			out.Connections[key] = check(a.comm.Verify)
			ok, detail := a.comm.Healthy()
			out.Ingress[key] = runtimeapi.CheckResult{OK: ok, Detail: detail}
		case a.tracker != nil:
			out.Connections[key] = check(a.tracker.Verify)
		case a.model != nil:
			out.Connections[key] = check(a.model.Verify)
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
