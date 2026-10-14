// Package credentials delivers explicitly sandbox-authorized credentials.
// Revocable credentials are encrypted and recorded durably before delivery;
// scope reductions and failed revocations survive process/replica changes.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/store"
)

var ErrForbidden = errors.New("credential not granted")

// Connections resolves the connection's current adapter. Revocation must be
// idempotent: retries include a remote success followed by a database failure.
type Connections interface {
	Tracker(orgID, key string) (connectors.Tracker, error)
}

// Ledger is shared by all replicas. Implementations must not fall back to
// process-local state when durability is unavailable.
type Ledger interface {
	Seat(context.Context, string) (*store.Seat, error)
	RecordIssuedCredential(context.Context, store.IssuedCredential, *access.SeatAccess) error
	ListIssuedCredentials(context.Context) ([]store.IssuedCredential, error)
	SetCredentialPending(context.Context, string) error
	ClaimCredentialRevocation(context.Context, string) (string, error)
	FinishCredentialRevocation(context.Context, string, string, bool) error
	CountIssuedCredentials(context.Context, string, string) (int, error)
}

// Registry issues credentials only when current, durably checked access
// permits it. EncryptionKey is a stable 32-byte key shared by all replicas,
// kept outside the database. Never rotate it while outstanding rows remain.
type Registry struct {
	Connections   Connections
	Ledger        Ledger
	EncryptionKey []byte
	Log           *slog.Logger
	Now           func() time.Time
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

// Scope returns the exact sandbox scope. Platform-only clauses never enter
// a sandbox credential. A single token cannot represent heterogeneous
// per-repository permissions, so that combination is rejected.
func Scope(a *access.SeatAccess, connection string) (connectors.CredentialScope, error) {
	g, err := a.GitHubSandboxGrant(connection)
	if err != nil {
		return connectors.CredentialScope{}, fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	if g != nil {
		return connectors.CredentialScope{Repos: slices.Clone(g.Repos), Permissions: clonePermissions(g.Permissions)}, nil
	}
	if a != nil && a.Browser != nil && a.Browser.SessionConnection == connection {
		return connectors.CredentialScope{}, nil
	}
	return connectors.CredentialScope{}, fmt.Errorf("%w: no access profile delivers %s to the sandbox; use connections.invoke for platform delivery", ErrForbidden, connection)
}

func clonePermissions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *Registry) cipher() (cipher.AEAD, error) {
	if len(r.EncryptionKey) != 32 {
		return nil, errors.New("sandbox credential delivery requires a 32-byte credential ledger encryption key")
	}
	block, err := aes.NewCipher(r.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func associatedData(i store.IssuedCredential) []byte {
	// Authenticate the immutable authorization record along with the token.
	b, _ := json.Marshal(struct {
		ID, Org, Seat, Connection string
		Scope                     connectors.CredentialScope
		Expires                   time.Time
	}{i.ID, i.OrganizationID, i.SeatID, i.Connection, i.Scope, i.ExpiresAt.UTC()})
	return b
}

func (r *Registry) seal(i store.IssuedCredential, credential connectors.Credential) ([]byte, error) {
	aead, err := r.cipher()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(credentialPayload{Token: credential.Token, Revocation: credential.Revocation})
	if err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, payload, associatedData(i)), nil
}

func (r *Registry) unseal(i store.IssuedCredential) (credentialPayload, error) {
	aead, err := r.cipher()
	if err != nil {
		return credentialPayload{}, err
	}
	n := aead.NonceSize()
	if len(i.Ciphertext) < n {
		return credentialPayload{}, errors.New("invalid encrypted credential record")
	}
	b, err := aead.Open(nil, i.Ciphertext[:n], i.Ciphertext[n:], associatedData(i))
	if err != nil {
		return credentialPayload{}, errors.New("cannot decrypt credential record with the configured ledger key")
	}
	var payload credentialPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		return credentialPayload{}, errors.New("invalid encrypted credential payload")
	}
	return payload, nil
}

// Issue persists a revocable credential before exposing it to the caller.
// seatID must be the authenticated stable seat UUID, never its reusable key.
// Access is read again before minting, and RecordIssuedCredential checks the
// same seat identity/access while holding a database lock before committing.
func (r *Registry) Issue(ctx context.Context, org, seatID string, a *access.SeatAccess, connection string) (runtimeapi.CredentialResponse, error) {
	if _, err := Scope(a, connection); err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	if r.Ledger == nil {
		return runtimeapi.CredentialResponse{}, errors.New("durable credential ledger is not configured")
	}
	if _, err := r.cipher(); err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	current, err := r.Ledger.Seat(ctx, seatID)
	if err != nil {
		return runtimeapi.CredentialResponse{}, err
	}
	if current.OrganizationID != org || current.ID != seatID {
		return runtimeapi.CredentialResponse{}, fmt.Errorf("%w: seat identity mismatch", ErrForbidden)
	}
	seat := current.Key
	scope, err := Scope(current.Manifest.Access, connection)
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
	if c.Revocable {
		if c.Token == "" || c.ExpiresAt.IsZero() || !c.ExpiresAt.After(r.now()) {
			if c.Token != "" {
				_ = revokeUndelivered(ctx, iss, c.Token)
			}
			return runtimeapi.CredentialResponse{}, errors.New("issuer returned a revocable credential without a valid token and expiry")
		}
		// PostgreSQL timestamps have microsecond precision; canonicalize before
		// authenticating the row so a database round trip preserves the AAD.
		record := store.IssuedCredential{ID: uuid.NewString(), OrganizationID: org, SeatID: current.ID, SeatKey: seat,
			Connection: connection, Scope: scope, ExpiresAt: c.ExpiresAt.UTC().Truncate(time.Microsecond)}
		record.Ciphertext, err = r.seal(record, c)
		if err == nil {
			err = r.Ledger.RecordIssuedCredential(ctx, record, current.Manifest.Access)
		}
		if err != nil {
			// A token that cannot be tracked must never be returned. If the database
			// is unavailable and remote cleanup also fails, this undisclosed token
			// can exist until expiry; remote mint+local commit are not one transaction.
			if revokeErr := revokeUndelivered(ctx, iss, c.Token); revokeErr != nil {
				r.log().Error("undelivered credential cleanup failed; it remains valid until expiry", "seat", seat, "connection", connection, "expires", c.ExpiresAt)
			}
			return runtimeapi.CredentialResponse{}, fmt.Errorf("persist issued credential: %w", err)
		}
	} else {
		// PATs and browser sessions cannot be scoped/revoked by this registry.
		// Recheck access after the provider call, including the stable seat ID.
		latest, err := r.Ledger.Seat(ctx, current.ID)
		if err != nil {
			return runtimeapi.CredentialResponse{}, err
		}
		if !sameAccess(current.Manifest.Access, latest.Manifest.Access) {
			return runtimeapi.CredentialResponse{}, fmt.Errorf("%w: access changed while issuing credential", ErrForbidden)
		}
	}
	return runtimeapi.CredentialResponse{Connection: connection, Username: c.Username, Token: c.Token, ExpiresAt: c.ExpiresAt, Revocable: c.Revocable, Data: c.Data}, nil
}

func sameAccess(a, b *access.SeatAccess) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// covers compares authority rather than grant presence. A repository removed
// from the sandbox or a permission weakened invalidates an old broader token.
func covers(current, issued connectors.CredentialScope) bool {
	for _, repo := range issued.Repos {
		if !slices.Contains(current.Repos, repo) {
			return false
		}
	}
	for permission, want := range issued.Permissions {
		got := current.Permissions[permission]
		if got != want && !(want == "read" && got == "write") {
			return false
		}
	}
	return true
}

// Revoked is the result of an attempted revocation, or a durable-ledger error.
type Revoked struct {
	Seat, Connection string
	Err              error
}

// Reconcile uses current committed access, not the caller's snapshot, so a
// stale sync on another replica cannot preserve a revoked scope. The seats
// argument is retained for source compatibility; the durable store is authoritative.
func (r *Registry) Reconcile(ctx context.Context, org string, _ map[string]*access.SeatAccess) []Revoked {
	return r.reconcile(ctx, org, "")
}

// RevokeSeat makes existing credentials pending even if access is later
// restored. seatID is the stable identity, never its reusable key. Retries
// continue in Sweep after the retirement call returns.
func (r *Registry) RevokeSeat(ctx context.Context, org, seatID string) []Revoked {
	return r.reconcile(ctx, org, seatID)
}

// Sweep checks all persisted credentials, including retired/deleted seats and
// pending failures. It runs at startup and periodically on every replica.
func (r *Registry) Sweep(ctx context.Context) []Revoked { return r.reconcile(ctx, "", "") }

func (r *Registry) reconcile(ctx context.Context, org, forceSeatID string) []Revoked {
	if r.Ledger == nil {
		return []Revoked{{Err: errors.New("durable credential ledger is not configured")}}
	}
	records, err := r.Ledger.ListIssuedCredentials(ctx)
	if err != nil {
		return []Revoked{{Err: err}}
	}
	var out []Revoked
	for _, i := range records {
		if org != "" && i.OrganizationID != org {
			continue
		}
		if forceSeatID != "" && i.SeatID != forceSeatID {
			continue
		}
		pending := i.Pending || forceSeatID != ""
		if !pending {
			seat, err := r.Ledger.Seat(ctx, i.SeatID)
			if errors.Is(err, store.ErrNotFound) {
				pending = true
			} else if err != nil {
				out = append(out, Revoked{Seat: i.SeatKey, Connection: i.Connection, Err: err})
				continue // Database failure is not proof of revoked authority.
			} else {
				scope, err := Scope(seat.Manifest.Access, i.Connection)
				pending = err != nil || !covers(scope, i.Scope)
			}
		}
		if !pending {
			continue
		}
		if err := r.Ledger.SetCredentialPending(ctx, i.ID); err != nil {
			out = append(out, Revoked{Seat: i.SeatKey, Connection: i.Connection, Err: err})
			continue
		}
		claimed, err := r.Ledger.ClaimCredentialRevocation(ctx, i.ID)
		if err != nil {
			out = append(out, Revoked{Seat: i.SeatKey, Connection: i.Connection, Err: err})
			continue
		}
		if claimed == "" {
			continue
		}
		err = r.revoke(ctx, i)
		if finishErr := r.Ledger.FinishCredentialRevocation(context.WithoutCancel(ctx), i.ID, claimed, err == nil); finishErr != nil {
			err = errors.Join(err, finishErr)
		}
		if err != nil {
			r.log().Warn("credential revocation remains pending and will be retried", "seat", i.SeatKey, "connection", i.Connection, "expires", i.ExpiresAt)
		} else {
			r.log().Info("credential revoked", "seat", i.SeatKey, "connection", i.Connection)
		}
		out = append(out, Revoked{Seat: i.SeatKey, Connection: i.Connection, Err: err})
	}
	return out
}

func (r *Registry) revoke(ctx context.Context, i store.IssuedCredential) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload, err := r.unseal(i)
	if err != nil {
		return err
	}
	if payload.Revocation != nil {
		return revokePersisted(ctx, *payload.Revocation, payload.Token)
	}
	t, err := r.Connections.Tracker(i.OrganizationID, i.Connection)
	if err != nil {
		return err
	}
	iss, ok := t.(connectors.CredentialIssuer)
	if !ok {
		return fmt.Errorf("connection %s cannot revoke credentials", i.Connection)
	}
	if err := iss.RevokeCredential(ctx, payload.Token); err != nil {
		// Provider errors can include requests/credentials; do not persist or expose
		// their text in revocation events. The row stays pending regardless.
		return errors.New("provider credential revocation failed")
	}
	return nil
}

// Outstanding counts unexpired revocable credentials. New callers should use
// the ledger count directly when they need to distinguish a database error.
func (r *Registry) Outstanding(org, seat string) int {
	if r.Ledger == nil {
		return 0
	}
	n, err := r.Ledger.CountIssuedCredentials(context.Background(), org, seat)
	if err != nil {
		r.log().Error("count outstanding credentials failed", "err", err)
		return 0
	}
	return n
}

// Cleanup is best effort and bounded even after a cancelled issuance request.
func revokeUndelivered(ctx context.Context, issuer connectors.CredentialIssuer, token string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return issuer.RevokeCredential(cleanup, token)
}
