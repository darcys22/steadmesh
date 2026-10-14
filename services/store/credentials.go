package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
)

// IssuedCredential is the durable revocation ledger for a sandbox credential.
// Ciphertext is an authenticated encryption envelope supplied by the caller;
// the store never receives the plaintext token. SeatID is the stable identity,
// not the reusable seat key. A zero ExpiresAt means no known expiry.
type IssuedCredential struct {
	ID             string
	OrganizationID string
	SeatID         string
	SeatKey        string
	Connection     string
	Scope          connectors.CredentialScope
	Ciphertext     []byte
	ExpiresAt      time.Time
	Pending        bool
}

const (
	// CredentialRevocationLease bounds one worker's exclusive retry claim.
	// The remote request must have a shorter timeout than this lease.
	CredentialRevocationLease = 30 * time.Second
	CredentialRevocationRetry = 10 * time.Second
)

// RecordIssuedCredential persists an already minted, encrypted credential.
// It checks the issuance snapshot while locking organisation then seat, the
// same order used by SyncOrganization and DeleteOrganization. A retiring seat
// can still issue during its wind-down; a retired/replaced identity cannot.
//
// A stale snapshot is committed as pending revocation and returns ErrConflict:
// the caller must not deliver the credential, but remote revocation can still
// be retried after a process crash. Other errors must also fail closed; a
// connection failure during commit can have an ambiguous persistence outcome.
func (s *Store) RecordIssuedCredential(ctx context.Context, record IssuedCredential, expectedAccess *access.SeatAccess) error {
	for _, id := range []string{record.ID, record.OrganizationID, record.SeatID} {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("%w: credential identity is not a UUID", ErrInvalid)
		}
	}
	if record.SeatKey == "" || record.Connection == "" || len(record.Ciphertext) == 0 {
		return fmt.Errorf("%w: incomplete issued credential", ErrInvalid)
	}
	expected, err := json.Marshal(expectedAccess)
	if err != nil {
		return err
	}
	scope, err := json.Marshal(record.Scope)
	if err != nil {
		return err
	}
	var expires *time.Time
	if !record.ExpiresAt.IsZero() {
		expires = &record.ExpiresAt
	}
	stale := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var active bool
		err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM organizations WHERE id = $1 FOR SHARE`, record.OrganizationID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			stale = true
		} else if err != nil {
			return err
		} else if !active {
			stale = true
		}
		if !stale {
			var raw []byte
			err = tx.QueryRow(ctx, `SELECT manifest FROM seats
				WHERE id = $1 AND organization_id = $2 AND key = $3 AND retired_at IS NULL FOR SHARE`,
				record.SeatID, record.OrganizationID, record.SeatKey).Scan(&raw)
			if errors.Is(err, pgx.ErrNoRows) {
				stale = true
			} else if err != nil {
				return err
			} else {
				var manifest compile.SeatManifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					return fmt.Errorf("credential seat manifest: %w", err)
				}
				current, err := json.Marshal(manifest.Access)
				if err != nil {
					return err
				}
				stale = !bytes.Equal(current, expected)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO issued_credentials
			(id, organization_id, seat_id, seat_key, connection_key, scope, ciphertext, expires_at, pending)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, record.ID, record.OrganizationID,
			record.SeatID, record.SeatKey, record.Connection, scope, record.Ciphertext, expires, record.Pending || stale)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return err
	}
	if stale {
		return ErrConflict
	}
	return nil
}

// ListIssuedCredentials returns every unexpired credential, including those
// whose organisation was deleted or seat retired. Expired credentials no
// longer need remote revocation and their encrypted token bytes are removed.
func (s *Store) ListIssuedCredentials(ctx context.Context) ([]IssuedCredential, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM issued_credentials WHERE expires_at <= now()`); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, organization_id, seat_id, seat_key, connection_key,
		scope, ciphertext, expires_at, pending FROM issued_credentials
		WHERE expires_at IS NULL OR expires_at > now() ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssuedCredential
	for rows.Next() {
		var record IssuedCredential
		var raw []byte
		var expires *time.Time
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.SeatID, &record.SeatKey,
			&record.Connection, &raw, &record.Ciphertext, &expires, &record.Pending); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &record.Scope); err != nil {
			return nil, fmt.Errorf("credential scope: %w", err)
		}
		if expires != nil {
			record.ExpiresAt = *expires
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// SetCredentialPending irreversibly schedules revocation. Repeated calls do
// not clear an active claim or postpone/accelerate the failed-attempt backoff.
func (s *Store) SetCredentialPending(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE issued_credentials SET
		next_attempt_at = CASE WHEN pending THEN next_attempt_at ELSE now() END,
		pending = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimCredentialRevocation returns an exclusive, fenced lease token, or an
// empty string if the credential is not pending, has expired, is backing off,
// or another worker owns it. A crashed worker's claim expires automatically.
func (s *Store) ClaimCredentialRevocation(ctx context.Context, id string) (string, error) {
	claimID := uuid.NewString()
	tag, err := s.pool.Exec(ctx, `UPDATE issued_credentials SET claim_id = $2,
		leased_until = now() + make_interval(secs => $3)
		WHERE id = $1 AND pending AND (expires_at IS NULL OR expires_at > now())
		AND next_attempt_at <= now() AND (leased_until IS NULL OR leased_until <= now())`,
		id, claimID, CredentialRevocationLease.Seconds())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", nil
	}
	return claimID, nil
}

// FinishCredentialRevocation forgets the ciphertext only after a successful
// remote revocation. Failure releases this claim and schedules a retry. A
// stale worker cannot finish a newer worker's claim. Error text and plaintext
// tokens are deliberately absent from this API and the ledger.
func (s *Store) FinishCredentialRevocation(ctx context.Context, id, claimID string, success bool) error {
	if claimID == "" {
		return ErrFenced
	}
	query := `UPDATE issued_credentials SET claim_id = NULL, leased_until = NULL,
		next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1 AND claim_id = $2`
	args := []any{id, claimID, CredentialRevocationRetry.Seconds()}
	if success {
		query = `DELETE FROM issued_credentials WHERE id = $1 AND claim_id = $2`
		args = args[:2]
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFenced
	}
	return nil
}

// CountIssuedCredentials counts unexpired entries, including pending retries.
func (s *Store) CountIssuedCredentials(ctx context.Context, org, seatKey string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM issued_credentials
		WHERE organization_id = $1 AND seat_key = $2 AND (expires_at IS NULL OR expires_at > now())`, org, seatKey).Scan(&count)
	return count, err
}

// pendingSeatCredentials runs under the seat's authority write lock so a
// revocation decision cannot be lost before the asynchronous worker sees it.
func pendingSeatCredentials(ctx context.Context, tx pgx.Tx, seatID string) error {
	_, err := tx.Exec(ctx, `UPDATE issued_credentials SET
		next_attempt_at = CASE WHEN pending THEN next_attempt_at ELSE now() END,
		pending = true WHERE seat_id = $1`, seatID)
	return err
}
