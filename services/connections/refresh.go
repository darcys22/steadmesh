package connections

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Credential refresh (docs/operations.html#credential-rotation).
//
// Every connection with a secret reference is re-resolved when the secret
// store reports a change (Kubernetes watch, debounced) and every
// RefreshInterval (Vault poll and backstop). Only a change of content (the
// digest) matters; a metadata-only update does nothing.
//
// New content is validated before it is used:
//   - valid: it replaces the current credential. In-flight requests finish
//     on the previous adapter; new requests use the new one. Communication
//     ingress is swapped make-before-break.
//   - definitively invalid (rejected as unauthorized, or the adapter cannot
//     be built from it): it is never activated. If the current credential
//     still works it stays in use for Grace, then the connection becomes
//     unavailable. A later secret version is validated again.
//   - transiently unverifiable: the current credential stays in use and the
//     refresh is retried; past MaxStale the connection becomes unavailable.
//
// A deleted secret keeps the current credential for Grace; a secret store
// that cannot be read keeps it for MaxStale.

func (m *Manager) startRefresh() {
	if m.opts.Secrets == nil {
		return
	}
	m.wg.Go(func() {
		t := time.NewTicker(m.opts.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-m.base.Done():
				return
			case <-t.C:
				m.refreshAll(m.base, "")
			}
		}
	})
	w, ok := m.opts.Secrets.(connectors.SecretWatcher)
	if !ok {
		return
	}
	var mu sync.Mutex
	pending := map[string]bool{}
	m.wg.Go(func() {
		if err := w.Watch(m.base, func(ref string) {
			mu.Lock()
			pending[ref] = true
			mu.Unlock()
		}); err != nil && m.base.Err() == nil {
			m.log.Warn("secret watch stopped; relying on periodic refresh", "error", err)
		}
	})
	m.wg.Go(func() {
		t := time.NewTicker(watchDebounce)
		defer t.Stop()
		for {
			select {
			case <-m.base.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			refs := pending
			pending = map[string]bool{}
			mu.Unlock()
			for ref := range refs {
				m.refreshAll(m.base, ref)
			}
		}
	})
}

// refreshAll refreshes every connection using ref, or every connection with a
// secret reference when ref is empty.
func (m *Manager) refreshAll(ctx context.Context, ref string) {
	m.refreshMatching(ctx, func(orgID string, c *conn) bool {
		return c.decl.SecretRef != "" && (ref == "" || c.decl.SecretRef == ref)
	})
}

// refreshOrg refreshes every connection of one organisation that has a
// secret reference.
func (m *Manager) refreshOrg(ctx context.Context, orgID string) {
	m.refreshMatching(ctx, func(o string, c *conn) bool { return o == orgID && c.decl.SecretRef != "" })
}

func (m *Manager) refreshMatching(ctx context.Context, match func(orgID string, c *conn) bool) {
	m.mu.Lock()
	var conns []*conn
	for orgID, org := range m.orgs {
		for _, c := range org {
			if match(orgID, c) {
				conns = append(conns, c)
			}
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		m.refresh(ctx, c)
	}
}

// RefreshNow re-resolves a connection's credential after an authentication
// failure. It reports whether a different credential is now in use, in
// which case the caller may retry a request that was definitely rejected.
// It runs at most once per ForcedRefreshInterval per connection.
func (m *Manager) RefreshNow(ctx context.Context, orgID, key string) bool {
	m.mu.Lock()
	c, ok := m.orgs[orgID][key]
	m.mu.Unlock()
	if !ok || c.decl.SecretRef == "" {
		return false
	}
	c.refreshMu.Lock()
	now := m.now()
	if !c.lastForced.IsZero() && now.Sub(c.lastForced) < ForcedRefreshInterval {
		c.refreshMu.Unlock()
		return false
	}
	c.lastForced = now
	c.refreshMu.Unlock()
	return m.refresh(ctx, c)
}

// refresh brings one connection's credential up to date. It reports whether
// a new credential was activated.
func (m *Manager) refresh(ctx context.Context, c *conn) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if !m.isCurrent(c) {
		return false
	}
	if c.decl.SecretRef == "" {
		// Nothing can change without a declaration change; retry the build.
		m.mu.Lock()
		usable := c.usable()
		m.mu.Unlock()
		if usable {
			return false
		}
		return m.tryCandidate(ctx, c, connectors.Resolved{})
	}
	res, err := m.opts.Secrets.Resolve(ctx, c.decl.SecretRef)
	now := m.now()
	if err != nil {
		m.resolveFailed(c, err, now)
		return false
	}

	m.mu.Lock()
	usable := c.usable()
	sameAsCurrent := usable && res.Digest == c.digest
	sameAsRejected := res.Digest == c.rejected
	if sameAsCurrent {
		// Unchanged content (perhaps a metadata-only update, or a deleted
		// secret restored as it was).
		c.cred = m.currentStatus(c.decl, res)
		c.rejected, c.failingSince, c.prevUntil = "", time.Time{}, time.Time{}
		m.mu.Unlock()
		return false
	}
	if sameAsRejected {
		// The rejected replacement is still the latest content.
		expired := usable && !c.prevUntil.IsZero() && !now.Before(c.prevUntil)
		m.mu.Unlock()
		if expired {
			m.deactivate(c, fmt.Errorf("replacement credential for %s was rejected and the previous credential's grace period ended: %s",
				c.key, c.cred.Error))
		}
		return false
	}
	m.mu.Unlock()
	return m.tryCandidate(ctx, c, res)
}

// tryCandidate builds and validates new secret content, activating it only
// when it is valid, or when nothing better is in use and it was not
// definitively rejected.
func (m *Manager) tryCandidate(ctx context.Context, c *conn, res connectors.Resolved) bool {
	now := m.now()
	cand, err := m.construct(c, res.Values)
	definitive := err != nil
	if err == nil {
		err = cand.verify(ctx)
		definitive = err != nil && (errors.Is(err, connectors.ErrUnauthorized) || errors.Is(err, connectors.ErrPermanent))
	}

	m.mu.Lock()
	usable := c.usable()
	current := c.adapters
	m.mu.Unlock()

	switch {
	case err == nil, !usable && !definitive:
		// Valid, or the best available: an unverifiable credential still
		// beats none, and Verify keeps reporting it.
		m.activate(c, cand, res)
		return true
	case definitive:
		m.log.Warn("replacement credential rejected", "organization_id", c.org, "connection", c.key, "error", err)
		detail := fmt.Sprintf("replacement credential (secret version %s) rejected: %v", res.Version, err)
		if !usable {
			m.mu.Lock()
			c.rejected = res.Digest
			c.err = errors.New(detail)
			c.cred = runtimeapi.CredentialStatus{State: runtimeapi.CredentialUnavailable, Error: detail}
			m.mu.Unlock()
			return false
		}
		if curErr := current.verify(ctx); curErr != nil &&
			(errors.Is(curErr, connectors.ErrUnauthorized) || errors.Is(curErr, connectors.ErrPermanent)) {
			m.mu.Lock()
			c.rejected = res.Digest
			m.mu.Unlock()
			m.deactivate(c, fmt.Errorf("%s; the previous credential is also rejected: %v", detail, curErr))
			return false
		}
		m.mu.Lock()
		c.rejected = res.Digest
		if c.prevUntil.IsZero() {
			c.prevUntil = now.Add(m.opts.Grace)
		}
		until := c.prevUntil
		c.cred.State = runtimeapi.CredentialReplacementRejected
		c.cred.Error = detail
		c.cred.PreviousUntil = &until
		m.mu.Unlock()
		return false
	default:
		// Transient: keep the current credential and retry later.
		m.markFailing(c, fmt.Errorf("validating replacement credential: %w", err), now)
		return false
	}
}

func (m *Manager) resolveFailed(c *conn, err error, now time.Time) {
	m.mu.Lock()
	usable := c.usable()
	m.mu.Unlock()
	if !usable {
		m.mu.Lock()
		c.err = fmt.Errorf("resolve secret for connection %s: %w", c.key, err)
		c.cred = runtimeapi.CredentialStatus{State: runtimeapi.CredentialUnavailable, Error: c.err.Error()}
		m.mu.Unlock()
		return
	}
	if errors.Is(err, connectors.ErrPermanent) {
		// The secret is gone or can never be read as referenced.
		m.mu.Lock()
		if c.prevUntil.IsZero() {
			c.prevUntil = now.Add(m.opts.Grace)
		}
		until := c.prevUntil
		c.cred.State = runtimeapi.CredentialSecretMissing
		c.cred.Error = err.Error()
		c.cred.PreviousUntil = &until
		m.mu.Unlock()
		if !now.Before(until) {
			m.deactivate(c, fmt.Errorf("secret for connection %s is missing and the grace period ended: %w", c.key, err))
		}
		return
	}
	m.markFailing(c, fmt.Errorf("reading secret: %w", err), now)
}

// markFailing records a transient refresh failure, disabling the connection
// once the current credential is older than MaxStale.
func (m *Manager) markFailing(c *conn, err error, now time.Time) {
	m.mu.Lock()
	if c.failingSince.IsZero() {
		c.failingSince = now
	}
	since := c.failingSince
	if c.cred.State != runtimeapi.CredentialReplacementRejected && c.cred.State != runtimeapi.CredentialSecretMissing {
		c.cred.State = runtimeapi.CredentialRefreshFailing
	}
	c.cred.Error = err.Error()
	c.cred.FailingSince = &since
	stale := now.Sub(since) >= m.opts.MaxStale
	m.mu.Unlock()
	if stale {
		m.deactivate(c, fmt.Errorf("credential for %s could not be refreshed for %s: %w", c.key, m.opts.MaxStale, err))
	}
}

// activate replaces the connection's adapters with cand. Communication
// ingress is swapped make-before-break: the new loop starts and is given
// HealthyWait to connect before the old one is stopped. Events are
// acknowledged only after they are accepted and are deduplicated by event
// ID, so the overlap neither drops nor duplicates them.
func (m *Manager) activate(c *conn, cand adapters, res connectors.Resolved) {
	var in *ingress
	if cand.comm != nil {
		in = m.runIngress(c.org, c.key, cand.comm)
		deadline := time.Now().Add(m.opts.HealthyWait)
		for ok, _ := cand.comm.Healthy(); !ok && time.Now().Before(deadline); ok, _ = cand.comm.Healthy() {
			select {
			case <-m.base.Done():
				in.stop()
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	m.mu.Lock()
	if m.orgs[c.org][c.key] != c {
		// Replaced by a declaration change while validating.
		m.mu.Unlock()
		in.stop()
		return
	}
	old := c.ingress
	c.adapters, c.ingress, c.err = cand, in, nil
	c.digest = res.Digest
	c.rejected, c.failingSince, c.prevUntil = "", time.Time{}, time.Time{}
	c.cred = m.currentStatus(c.decl, res)
	m.mu.Unlock()
	old.stop()
	m.log.Info("connection credential activated", "organization_id", c.org, "connection", c.key, "secret_version", res.Version)
}

// deactivate makes a connection unavailable with reason.
func (m *Manager) deactivate(c *conn, reason error) {
	m.mu.Lock()
	if m.orgs[c.org][c.key] != c {
		m.mu.Unlock()
		return
	}
	old := c.ingress
	c.adapters, c.ingress = adapters{}, nil
	c.err = reason
	c.cred = runtimeapi.CredentialStatus{State: runtimeapi.CredentialUnavailable, Error: reason.Error()}
	m.mu.Unlock()
	old.stop()
	m.log.Warn("connection disabled", "organization_id", c.org, "connection", c.key, "reason", reason)
}

func (m *Manager) isCurrent(c *conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.orgs[c.org][c.key] == c
}
