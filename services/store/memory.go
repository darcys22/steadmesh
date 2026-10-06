package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Every memory query takes the caller's authorised store ids and filters on
// them in SQL, so records, counts, titles and snippets of other stores are
// never read on the caller's behalf (§8.3, A12).

// ActiveStoreIDs maps the given store keys to the ids of their active stores.
// Unknown or retired keys are omitted.
func (s *Store) ActiveStoreIDs(ctx context.Context, orgID string, keys []string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, id FROM memory_stores
		WHERE organization_id = $1 AND key = ANY($2) AND retired_at IS NULL`, orgID, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// Record is the current revision of a memory record.
type Record struct {
	ID         string    `json:"record_id"`
	Store      string    `json:"store"`
	StoreID    string    `json:"-"`
	Revision   int       `json:"revision"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Tags       []string  `json:"tags,omitempty"`
	SourceRefs []string  `json:"source_refs,omitempty"`
	Author     string    `json:"author_seat"`
	Archived   bool      `json:"archived,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RevisionConflict reports a stale expected revision (A11).
type RevisionConflict struct {
	Current       int
	CurrentAuthor string
}

func (e *RevisionConflict) Error() string {
	return fmt.Sprintf("revision conflict: current revision is %d", e.Current)
}

func (e *RevisionConflict) Is(target error) bool { return target == ErrConflict }

const recordColumns = `r.id, ms.key, r.store_id, r.revision, r.title, r.body, r.tags, r.source_refs, a.key, r.archived, r.created_at, r.updated_at
	FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id JOIN seats a ON a.id = r.author_seat_id`

func scanRecord(row pgx.Row) (*Record, error) {
	r := &Record{}
	err := row.Scan(&r.ID, &r.Store, &r.StoreID, &r.Revision, &r.Title, &r.Body, &r.Tags, &r.SourceRefs, &r.Author,
		&r.Archived, &r.CreatedAt, &r.UpdatedAt)
	return r, notFound(err)
}

// Record reads a record from one of the authorised stores.
func (s *Store) Record(ctx context.Context, orgID, recordID string, storeIDs []string) (*Record, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	return scanRecord(s.pool.QueryRow(ctx, `SELECT `+recordColumns+`
		WHERE r.organization_id = $1 AND r.id = $2 AND r.store_id = ANY($3)`, orgID, recordID, storeIDs))
}

// RecordRevision reads a past revision of a record in an authorised store.
func (s *Store) RecordRevision(ctx context.Context, orgID, recordID string, revision int, storeIDs []string) (*Record, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	return scanRecord(s.pool.QueryRow(ctx, `SELECT v.record_id, ms.key, v.store_id, v.revision, v.title, v.body, v.tags,
			v.source_refs, a.key, v.archived, r.created_at, v.created_at
		FROM memory_revisions v JOIN memory_records r ON r.id = v.record_id
		JOIN memory_stores ms ON ms.id = v.store_id JOIN seats a ON a.id = v.author_seat_id
		WHERE r.organization_id = $1 AND v.record_id = $2 AND v.revision = $3 AND v.store_id = ANY($4)`,
		orgID, recordID, revision, storeIDs))
}

// SearchHit is a bounded, attributed search result.
type SearchHit struct {
	RecordID  string    `json:"record_id"`
	Store     string    `json:"store"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet"`
	Tags      []string  `json:"tags,omitempty"`
	Revision  int       `json:"revision"`
	Author    string    `json:"author_seat"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Search runs a Postgres full-text query (websearch syntax) over the
// authorised stores. An empty query lists the most recently updated records.
func (s *Store) Search(ctx context.Context, orgID string, storeIDs []string, query string, tags []string, offset, limit int) ([]SearchHit, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, ms.key, r.title,
			CASE WHEN $3 = '' THEN left(r.body, 200)
			     ELSE ts_headline('english', r.body, websearch_to_tsquery('english', $3), 'MaxFragments=2,MaxWords=25,MinWords=8') END,
			r.tags, r.revision, a.key, r.updated_at
		FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id JOIN seats a ON a.id = r.author_seat_id
		WHERE r.organization_id = $1 AND r.store_id = ANY($2) AND NOT r.archived
		  AND ($3 = '' OR r.search @@ websearch_to_tsquery('english', $3))
		  AND (cardinality($4::text[]) = 0 OR r.tags @> $4::text[])
		ORDER BY CASE WHEN $3 = '' THEN 0 ELSE ts_rank(r.search, websearch_to_tsquery('english', $3)) END DESC,
			r.updated_at DESC, r.id
		OFFSET $5 LIMIT $6`, orgID, storeIDs, query, nonNil(tags), offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SearchHit, error) {
		var h SearchHit
		err := r.Scan(&h.RecordID, &h.Store, &h.Title, &h.Snippet, &h.Tags, &h.Revision, &h.Author, &h.UpdatedAt)
		return h, err
	})
}

// NewRecord is the content of a new memory record.
type NewRecord struct {
	OrganizationID string
	StoreID        string
	AuthorID       string
	Title          string
	Body           string
	Tags           []string
	SourceRefs     []string
}

// WriteRecord creates a record at revision 1 with its first history entry.
func (s *Store) WriteRecord(ctx context.Context, f Fence, in NewRecord) (*Record, error) {
	id := uuid.NewString()
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, organization_id, store_id, revision, title, body, tags, author_seat_id, source_refs)
			VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $8)`, id, in.OrganizationID, in.StoreID, in.Title, in.Body,
			nonNil(in.Tags), in.AuthorID, nonNil(in.SourceRefs)); err != nil {
			return err
		}
		return appendRevision(ctx, tx, id, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, in.OrganizationID, id, []string{in.StoreID})
}

// RecordChange is a compare-and-swap update of a record. Nil fields keep their
// current value.
type RecordChange struct {
	OrganizationID   string
	RecordID         string
	AuthorID         string
	ExpectedRevision int
	Title            *string
	Body             *string
	Tags             []string
	SourceRefs       []string
	Archive          bool
}

// ReviseRecord applies a change only if the record is still at the expected
// revision; otherwise it returns a *RevisionConflict naming the current one.
func (s *Store) ReviseRecord(ctx context.Context, f Fence, in RecordChange, storeIDs []string) (*Record, error) {
	if _, err := uuid.Parse(in.RecordID); err != nil {
		return nil, ErrNotFound
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, title = COALESCE($5, title),
				body = COALESCE($6, body), tags = COALESCE($7, tags), source_refs = COALESCE($8, source_refs),
				archived = archived OR $9, author_seat_id = $10, updated_at = now()
			WHERE organization_id = $1 AND id = $2 AND store_id = ANY($3) AND revision = $4`,
			in.OrganizationID, in.RecordID, storeIDs, in.ExpectedRevision, in.Title, in.Body, in.Tags, in.SourceRefs,
			in.Archive, in.AuthorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			conflict := &RevisionConflict{}
			err := tx.QueryRow(ctx, `SELECT r.revision, a.key FROM memory_records r JOIN seats a ON a.id = r.author_seat_id
				WHERE r.organization_id = $1 AND r.id = $2 AND r.store_id = ANY($3)`, in.OrganizationID, in.RecordID, storeIDs).
				Scan(&conflict.Current, &conflict.CurrentAuthor)
			if err != nil {
				return notFound(err)
			}
			return conflict
		}
		return appendRevision(ctx, tx, in.RecordID, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, in.OrganizationID, in.RecordID, storeIDs)
}

// appendRevision copies the record's current state into its append-only
// history with the author's lease generation.
func appendRevision(ctx context.Context, tx pgx.Tx, recordID string, gen int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO memory_revisions (record_id, revision, store_id, title, body, tags, author_seat_id,
			author_generation, source_refs, archived)
		SELECT id, revision, store_id, title, body, tags, author_seat_id, $2, source_refs, archived FROM memory_records WHERE id = $1`,
		recordID, gen)
	return err
}

// RevisionInfo is one entry of a record's history (body omitted).
type RevisionInfo struct {
	Revision   int       `json:"revision"`
	Title      string    `json:"title"`
	Author     string    `json:"author_seat"`
	Generation *int64    `json:"author_generation,omitempty"`
	SourceRefs []string  `json:"source_refs,omitempty"`
	Archived   bool      `json:"archived,omitempty"`
	BodyBytes  int       `json:"body_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

// History pages a record's revisions, newest first.
func (s *Store) History(ctx context.Context, orgID, recordID string, storeIDs []string, offset, limit int) ([]RevisionInfo, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory_records WHERE organization_id = $1 AND id = $2 AND store_id = ANY($3))`,
		orgID, recordID, storeIDs).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT v.revision, v.title, a.key, v.author_generation, v.source_refs, v.archived,
			octet_length(v.body), v.created_at
		FROM memory_revisions v JOIN seats a ON a.id = v.author_seat_id
		WHERE v.record_id = $1 ORDER BY v.revision DESC OFFSET $2 LIMIT $3`, recordID, offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RevisionInfo, error) {
		var v RevisionInfo
		err := r.Scan(&v.Revision, &v.Title, &v.Author, &v.Generation, &v.SourceRefs, &v.Archived, &v.BodyBytes, &v.CreatedAt)
		return v, err
	})
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// IsConflict extracts a revision conflict.
func IsConflict(err error) (*RevisionConflict, bool) {
	var c *RevisionConflict
	ok := errors.As(err, &c)
	return c, ok
}
