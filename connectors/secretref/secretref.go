// Package secretref resolves connection secret references
// (docs/architecture.html#secrets): "k8s:<name>" reads a Secret in the
// control-plane namespace and "vault:<path>" reads Vault KV v2 using
// Kubernetes auth.
// Secret values are never logged or included in errors.
package secretref

import (
	"context"
	"fmt"
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
func (m Multi) Resolve(ctx context.Context, ref string) (map[string]string, error) {
	scheme, _, err := Split(ref)
	if err != nil {
		return nil, err
	}
	r, ok := m[scheme]
	if !ok {
		return nil, fmt.Errorf("%w: no resolver configured for secret scheme %q", connectors.ErrPermanent, scheme)
	}
	return r.Resolve(ctx, ref)
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
