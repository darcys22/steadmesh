-- +goose Up
-- Revocation survives process restarts, seat retirement/recreation and even
-- organisation purge. Deliberately no cascading foreign keys: deleting the
-- authority must not delete the only handle that can revoke an issued token.
-- RecordIssuedCredential verifies the stable IDs under the authority locks.
CREATE TABLE issued_credentials (
    id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    seat_id uuid NOT NULL,
    seat_key text NOT NULL,
    connection_key text NOT NULL,
    scope jsonb NOT NULL,
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) > 0),
    expires_at timestamptz,
    pending boolean NOT NULL DEFAULT false,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_id uuid,
    leased_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((claim_id IS NULL AND leased_until IS NULL)
        OR (pending AND claim_id IS NOT NULL AND leased_until IS NOT NULL))
);
CREATE INDEX issued_credentials_seat ON issued_credentials (organization_id, seat_id);
CREATE INDEX issued_credentials_pending ON issued_credentials (next_attempt_at) WHERE pending;
CREATE INDEX issued_credentials_expiry ON issued_credentials (expires_at) WHERE expires_at IS NOT NULL;

-- +goose Down
DROP TABLE issued_credentials;
