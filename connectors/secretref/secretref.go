// Package secretref resolves connection secret references
// (docs/architecture.html#secrets): "k8s:<name>" reads a Secret in the
// control-plane namespace and "vault:<path>" reads Vault KV v2 using
// Kubernetes auth.
// Secret values are never logged or included in errors.
package secretref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/darcys22/steadmesh/connectors"
)

// Reference schemes.
const (
	SchemeKubernetes = "k8s"
	SchemeVault      = "vault"
)

// Split parses "<scheme>:<name>".
func Split(ref string) (scheme, name string, err error) {
	scheme, name, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("%w: malformed secret reference %q", connectors.ErrPermanent, ref)
	}
	return scheme, name, nil
}

// Digest identifies secret content. It changes exactly when a key or value
// changes, independently of the store's version numbering.
func Digest(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(values[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func resolved(values map[string]string, version string) connectors.Resolved {
	return connectors.Resolved{Values: values, Version: version, Digest: Digest(values)}
}

// Multi dispatches a reference to the resolver registered for its scheme.
type Multi map[string]connectors.Secrets

// NewMulti returns a resolver dispatching by scheme ("k8s", "vault"). Nil
// entries are ignored.
func NewMulti(resolvers map[string]connectors.Secrets) Multi {
	m := Multi{}
	for k, v := range resolvers {
		if v != nil {
			m[k] = v
		}
	}
	return m
}

// Resolve implements connectors.Secrets.
func (m Multi) Resolve(ctx context.Context, ref string) (connectors.Resolved, error) {
	scheme, _, err := Split(ref)
	if err != nil {
		return connectors.Resolved{}, err
	}
	r, ok := m[scheme]
	if !ok {
		return connectors.Resolved{}, fmt.Errorf("%w: no resolver configured for secret scheme %q", connectors.ErrPermanent, scheme)
	}
	return r.Resolve(ctx, ref)
}

// Watch runs every resolver that can watch until ctx is cancelled. Resolvers
// without a watch are covered by the caller's periodic refresh.
func (m Multi) Watch(ctx context.Context, notify func(ref string)) error {
	errc := make(chan error, len(m))
	n := 0
	for _, r := range m {
		if w, ok := r.(connectors.SecretWatcher); ok {
			n++
			go func() { errc <- w.Watch(ctx, notify) }()
		}
	}
	if n == 0 {
		<-ctx.Done()
		return nil
	}
	var first error
	for range n {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func expect(ref, scheme string) (string, error) {
	s, name, err := Split(ref)
	if err != nil {
		return "", err
	}
	if s != scheme {
		return "", fmt.Errorf("%w: secret reference %q is not a %s: reference", connectors.ErrPermanent, ref, scheme)
	}
	return name, nil
}
