// Package browsersession holds a signed-in browser session for the browser
// access plugin: a Playwright storage state (cookies and local storage),
// secret key storage_state. Producing it is a manual login with a dedicated
// test account (docs/sandbox.html). It is delivered to the sandbox only for
// seats whose browser access names the connection; Steadmesh cannot
// invalidate cookies already delivered: log the account out or rotate the
// secret.
package browsersession

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/darcys22/steadmesh/connectors"
)

// Adapter implements connectors.Tracker (no operations) and
// connectors.CredentialIssuer.
type Adapter struct{ state json.RawMessage }

var (
	_ connectors.Tracker          = (*Adapter)(nil)
	_ connectors.CredentialIssuer = (*Adapter)(nil)
)

// New validates the storage state.
func New(cfg connectors.Config) (connectors.Tracker, error) {
	raw := cfg.Secret["storage_state"]
	var st struct {
		Cookies []json.RawMessage `json:"cookies"`
		Origins []json.RawMessage `json:"origins"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &st) != nil || st.Cookies == nil {
		return nil, fmt.Errorf("%w: browser_session secret must contain storage_state, a Playwright storage state with cookies", connectors.ErrUnauthorized)
	}
	return &Adapter{state: json.RawMessage(raw)}, nil
}

// Verify succeeds: the state was validated when the adapter was built.
func (a *Adapter) Verify(context.Context) error { return nil }

func (a *Adapter) Invoke(context.Context, string, json.RawMessage, string) (connectors.Result, error) {
	return connectors.Result{}, fmt.Errorf("%w: a browser session has no operations", connectors.ErrPermanent)
}

func (a *Adapter) FindByOperation(context.Context, string, json.RawMessage, string) (*connectors.Result, error) {
	return nil, nil
}

func (a *Adapter) ReadOnly(string) bool { return true }

// IssueCredential returns the storage state.
func (a *Adapter) IssueCredential(context.Context, connectors.CredentialScope) (connectors.Credential, error) {
	return connectors.Credential{Data: a.state}, nil
}

// RevokeCredential is not possible: cookies stay valid until the account is
// logged out.
func (a *Adapter) RevokeCredential(context.Context, string) error {
	return fmt.Errorf("%w: browser cookies can only be invalidated by logging the account out", connectors.ErrPermanent)
}
