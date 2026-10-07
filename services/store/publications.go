package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Work publication states.
const (
	PubPending   = "pending"
	PubPublished = "published"
	// PubBlocked: the connection is unavailable; retried with backoff. It
	// never affects the work item itself.
	PubBlocked = "blocked"
	PubFailed  = "failed"
)

// PublicationItem is a claimed publication with the work item to publish.
type PublicationItem struct {
	OrganizationID    string
	RecordID          string
	Connection        string
	ExternalKind      string
	ExternalID        string
	ExternalURL       string
	PublishedRevision int
	Attempts          int
	Revision          int
	Data              json.RawMessage
	Body              string
}

// EnqueuePublications makes sure every live work item in the stores has a
// publication row for connection.
func (s *Store) EnqueuePublications(ctx context.Context, orgID string, storeIDs []string, connection, kind string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_publications (organization_id, record_id, connection, external_kind)
		SELECT r.organization_id, r.id, $3, $4 FROM memory_records r
		WHERE r.organization_id = $1 AND r.store_id = ANY($2) AND r.kind = 'work' AND NOT r.archived
		ON CONFLICT DO NOTHING`, orgID, storeIDs, connection, kind)
	return err
}

// ClaimPublications reserves, for lease, up to limit publications that are
// due: never published, or behind the work item's current revision. A
// publisher that dies holding a claim is retried once the lease ends.
func (s *Store) ClaimPublications(ctx context.Context, orgID, connection string, limit int, lease time.Duration) ([]PublicationItem, error) {
	var out []PublicationItem
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.organization_id, p.record_id, p.connection, p.external_kind, p.external_id,
				p.external_url, p.published_revision, p.attempts, r.revision, r.data, r.body
			FROM work_publications p JOIN memory_records r ON r.id = p.record_id
			WHERE p.organization_id = $1 AND p.connection = $2 AND p.next_attempt_at <= now()
			  AND (p.external_id = '' OR r.revision > p.published_revision)
			ORDER BY p.next_attempt_at LIMIT $3 FOR UPDATE OF p SKIP LOCKED`, orgID, connection, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PublicationItem, error) {
			var it PublicationItem
			var data []byte
			err := r.Scan(&it.OrganizationID, &it.RecordID, &it.Connection, &it.ExternalKind, &it.ExternalID, &it.ExternalURL,
				&it.PublishedRevision, &it.Attempts, &it.Revision, &data, &it.Body)
			it.Data = data
			return it, err
		})
		if err != nil {
			return err
		}
		for i := range out {
			out[i].Attempts++
			if _, err := tx.Exec(ctx, `UPDATE work_publications SET attempts = attempts + 1,
				next_attempt_at = now() + make_interval(secs => $5), updated_at = now()
				WHERE organization_id = $1 AND record_id = $2 AND connection = $3 AND external_kind = $4`,
				out[i].OrganizationID, out[i].RecordID, out[i].Connection, out[i].ExternalKind, lease.Seconds()); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// SetPublicationExternal records the external object a publication created,
// before anything else, so it is never created twice.
func (s *Store) SetPublicationExternal(ctx context.Context, it PublicationItem, externalID, url string) error {
	_, err := s.pool.Exec(ctx, `UPDATE work_publications SET external_id = $5, external_url = $6, updated_at = now()
		WHERE organization_id = $1 AND record_id = $2 AND connection = $3 AND external_kind = $4`,
		it.OrganizationID, it.RecordID, it.Connection, it.ExternalKind, externalID, url)
	return err
}

// ResolvePublication records the outcome of an attempt. A published
// revision resets the attempt count; retryIn schedules the next attempt.
func (s *Store) ResolvePublication(ctx context.Context, it PublicationItem, state string, publishedRevision int, errText string, retryIn time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE work_publications SET state = $5,
			published_revision = GREATEST(published_revision, $6), last_error = $7,
			attempts = CASE WHEN $5 = 'published' THEN 0 ELSE attempts END,
			next_attempt_at = now() + make_interval(secs => $8), updated_at = now()
		WHERE organization_id = $1 AND record_id = $2 AND connection = $3 AND external_kind = $4`,
		it.OrganizationID, it.RecordID, it.Connection, it.ExternalKind, state, publishedRevision, errText, retryIn.Seconds())
	return err
}

// Publication is the status of one work item's publication.
type Publication struct {
	RecordID          string    `json:"record_id"`
	Path              string    `json:"path"`
	Store             string    `json:"store"`
	Connection        string    `json:"connection"`
	ExternalID        string    `json:"external_id,omitempty"`
	ExternalURL       string    `json:"external_url,omitempty"`
	State             string    `json:"state"`
	PublishedRevision int       `json:"published_revision"`
	Revision          int       `json:"revision"`
	Attempts          int       `json:"attempts"`
	LastError         string    `json:"last_error,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Publications lists an organisation's publications, most recently changed
// first.
func (s *Store) Publications(ctx context.Context, orgID string, limit int) ([]Publication, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.record_id, r.path, ms.key, p.connection, p.external_id, p.external_url, p.state,
			p.published_revision, r.revision, p.attempts, p.last_error, p.updated_at
		FROM work_publications p JOIN memory_records r ON r.id = p.record_id JOIN memory_stores ms ON ms.id = r.store_id
		WHERE p.organization_id = $1 ORDER BY p.updated_at DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Publication])
}

// BeginSystemOperation records a pending platform-initiated operation (no
// seat, no lease fence). A reused idempotency key returns the recorded
// operation with created=false; it must not be re-executed blindly.
func (s *Store) BeginSystemOperation(ctx context.Context, in NewOperation) (*runtimeapi.Operation, bool, error) {
	id := uuid.NewString()
	tag, err := s.pool.Exec(ctx, `INSERT INTO connector_operations (id, organization_id, connection, operation, target,
			request_hash, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (organization_id, connection, idempotency_key) DO NOTHING`,
		id, in.OrganizationID, in.Connection, in.Operation, in.Target, in.RequestHash, in.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	op, err := scanOperation(s.pool.QueryRow(ctx, `SELECT `+operationColumns+` FROM connector_operations
		WHERE organization_id = $1 AND connection = $2 AND idempotency_key = $3`,
		in.OrganizationID, in.Connection, in.IdempotencyKey))
	return op, tag.RowsAffected() == 1, err
}
