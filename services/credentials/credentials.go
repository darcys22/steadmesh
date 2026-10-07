// Package credentials delivers credentials into sandboxes for seats whose
// access grants sandbox delivery (the github and browser plugins), and
// revokes them when the grant goes (docs/sandbox.html).
//
// Revocation has two parts. Steadmesh stops delivering at once: a seat
// without the grant is refused. Credentials already delivered are
// invalidated where the service allows it: GitHub App installation tokens
// are revoked at GitHub when a sync removes the seat's grant. A personal
// access token and browser cookies cannot be invalidated by Steadmesh; they
// stay usable until rotated or logged out, which the documentation states.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// ErrForbidden means the seat's access does not grant the credential.
var ErrForbidden = errors.New("credential not granted")

// Connections resolves a connection's adapter.
type Connections interface {
	Tracker(orgID, key string) (connectors.Tracker, error)
}

// Registry issues credentials and remembers revocable ones.
type Registry struct {
	Connections Connections
	Log         *slog.Logger
	Now         func() time.Time

	mu     sync.Mutex
	issued map[seatRef][]issued
}

type seatRef struct{ org, seat string }

type issued struct {
	connection string
	token      string
	expires    time.Time
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Scope is what a seat's access grants on a connection, or ErrForbidden.
func Scope(a *access.SeatAccess, connection string) (connectors.CredentialScope, error) {
	if g := a.GitHubFor(connection); g != nil {
		if g.Delivery != access.DeliverySandbox {
			return connectors.CredentialScope{}, fmt.Errorf("%w: the seat's github access on %s is platform-delivered; use connections.invoke", ErrForbidden, connection)
		}
		return connectors.CredentialScope{Repos: g.Repos, Permissions: g.Permissions}, nil
	}
	if a != nil && a.Browser != nil && a.Browser.SessionConnection == connection {
		return connectors.CredentialScope{}, nil
	}
	return connectors.CredentialScope{}, fmt.Errorf("%w: no access profile of the seat delivers credentials of %s to its sandbox", ErrForbidden, connection)
}

// Issue delivers a credential of connection to seat (key) of org.
func (r *Registry) Issue(ctx context.Context, org, seat string, a *access.SeatAccess, connection string) (runtimeapi.CredentialResponse, error) {
	scope, err := Scope(a, connection)
	if err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	t, err := r.Connections.Tracker(org, connection)
	if err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	iss, ok := t.(connectors.CredentialIssuer)
	if !ok {
		return runtimeapi.CredentialResponse{}, fmt.Errorf("%w: connection %s cannot deliver credentials", ErrForbidden, connection)
	}
	c, err := iss.IssueCredential(ctx, scope)
	if err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	if c.Revocable && c.Token != "" {
		r.mu.Lock()
		if r.issued == nil {
			r.issued = map[seatRef][]issued{}
		}
		ref := seatRef{org, seat}
		r.issued[ref] = append(r.prune(r.issued[ref]), issued{connection: connection, token: c.Token, expires: c.ExpiresAt})
		r.mu.Unlock()
	}
	return runtimeapi.CredentialResponse{Connection: connection, Username: c.Username, Token: c.Token, ExpiresAt: c.ExpiresAt, Revocable: c.Revocable, Data: c.Data}, nil
}

// prune drops expired entries. Callers hold r.mu.
func (r *Registry) prune(in []issued) []issued {
	now := r.now()
	out := in[:0]
	for _, i := range in {
		if i.expires.IsZero() || i.expires.After(now) {
			out = append(out, i)
		}
	}
	return out
}

// Revoked is one revoked credential, for events.
type Revoked struct {
	Seat, Connection string
	Err              error
}

// Reconcile revokes the credentials of org's seats that no longer hold the
// grant they were issued under. seats maps seat key to current access; a
// missing seat lost everything.
func (r *Registry) Reconcile(ctx context.Context, org string, seats map[string]*access.SeatAccess) []Revoked {
	r.mu.Lock()
	var todo []struct {
		ref seatRef
		i   issued
	}
	for ref, list := range r.issued {
		if ref.org != org {
			continue
		}
		var keep []issued
		for _, i := range r.prune(list) {
			if _, err := Scope(seats[ref.seat], i.connection); err != nil {
				todo = append(todo, struct {
					ref seatRef
					i   issued
				}{ref, i})
				continue
			}
			keep = append(keep, i)
		}
		if len(keep) == 0 {
			delete(r.issued, ref)
		} else {
			r.issued[ref] = keep
		}
	}
	r.mu.Unlock()
	var out []Revoked
	for _, t := range todo {
		err := r.revoke(ctx, org, t.i)
		if err != nil {
			r.log().Warn("credential revocation failed; it expires on its own", "seat", t.ref.seat, "connection", t.i.connection, "expires", t.i.expires, "err", err)
		} else {
			r.log().Info("credential revoked", "seat", t.ref.seat, "connection", t.i.connection)
		}
		out = append(out, Revoked{Seat: t.ref.seat, Connection: t.i.connection, Err: err})
	}
	return out
}

func (r *Registry) revoke(ctx context.Context, org string, i issued) error {
	t, err := r.Connections.Tracker(org, i.connection)
	if err != nil {
		return err
	}
	iss, ok := t.(connectors.CredentialIssuer)
	if !ok {
		return fmt.Errorf("connection %s cannot revoke credentials", i.connection)
	}
	return iss.RevokeCredential(ctx, i.token)
}

// Outstanding counts unexpired revocable credentials of a seat.
func (r *Registry) Outstanding(org, seat string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prune(r.issued[seatRef{org, seat}]))
}
