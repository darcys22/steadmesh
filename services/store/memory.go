package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Every memory query takes the caller's authorised store ids and filters on
// them in SQL, so records, counts, paths and snippets of other stores are
// never read on the caller's behalf (§8.3, A12).
//
// Records are addressed like files: a path unique within its store. Notes
// (kind "note") are free-form. Work items (kind "work") carry structured
// data and are changed only through ReviseStructured, which the work tools
// use to enforce ownership and status rules; every other write path rejects
// them with ErrStructured.

// Record kinds.
const (
	KindNote = "note"
	KindWork = "work"
)

// ErrStructured means a plain memory write targeted a structured record.
var ErrStructured = errors.New("structured record")

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
	ID         string          `json:"record_id"`
	Store      string          `json:"store"`
	StoreID    string          `json:"-"`
	Revision   int             `json:"revision"`
	Path       string          `json:"path"`
	Kind       string          `json:"kind"`
	Data       json.RawMessage `json:"-"`
	Text       string          `json:"text"`
	Tags       []string        `json:"tags,omitempty"`
	SourceRefs []string        `json:"source_refs,omitempty"`
	Author     string          `json:"author_seat"`
	Archived   bool            `json:"archived,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
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

const recordColumns = `r.id, ms.key, r.store_id, r.revision, r.path, r.kind, r.data, r.body, r.tags, r.source_refs, a.key,
	r.archived, r.created_at, r.updated_at
	FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id JOIN seats a ON a.id = r.author_seat_id`

func scanRecord(row pgx.Row) (*Record, error) {
	r := &Record{}
	var data []byte
	err := row.Scan(&r.ID, &r.Store, &r.StoreID, &r.Revision, &r.Path, &r.Kind, &data, &r.Text, &r.Tags, &r.SourceRefs,
		&r.Author, &r.Archived, &r.CreatedAt, &r.UpdatedAt)
	r.Data = data
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

// RecordByPath reads the live record at path in an authorised store.
func (s *Store) RecordByPath(ctx context.Context, orgID, storeID, path string) (*Record, error) {
	return scanRecord(s.pool.QueryRow(ctx, `SELECT `+recordColumns+`
		WHERE r.organization_id = $1 AND r.store_id = $2 AND r.path = $3 AND NOT r.archived`, orgID, storeID, path))
}

// RecordRevision reads a past revision of a record in an authorised store.
func (s *Store) RecordRevision(ctx context.Context, orgID, recordID string, revision int, storeIDs []string) (*Record, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	return scanRecord(s.pool.QueryRow(ctx, `SELECT v.record_id, ms.key, v.store_id, v.revision, v.path, r.kind, v.data,
			v.body, v.tags, v.source_refs, a.key, v.archived, r.created_at, v.created_at
		FROM memory_revisions v JOIN memory_records r ON r.id = v.record_id
		JOIN memory_stores ms ON ms.id = v.store_id JOIN seats a ON a.id = v.author_seat_id
		WHERE r.organization_id = $1 AND v.record_id = $2 AND v.revision = $3 AND v.store_id = ANY($4)`,
		orgID, recordID, revision, storeIDs))
}

// Entry is a record's listing line (no body).
type Entry struct {
	RecordID  string    `json:"record_id"`
	Store     string    `json:"store"`
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Revision  int       `json:"revision"`
	Bytes     int       `json:"bytes"`
	Author    string    `json:"author_seat"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListOrder selects the listing order column.
type ListOrder string

// Listing orders.
const (
	OrderName      ListOrder = "name"
	OrderCreatedAt ListOrder = "created_at"
	OrderUpdatedAt ListOrder = "updated_at"
)

// List pages the live records of the authorised stores whose path starts
// with prefix.
func (s *Store) List(ctx context.Context, orgID string, storeIDs []string, prefix string, order ListOrder, desc bool, offset, limit int) ([]Entry, error) {
	col := map[ListOrder]string{OrderName: "r.path", OrderCreatedAt: "r.created_at", OrderUpdatedAt: "r.updated_at"}[order]
	if col == "" {
		col = "r.path"
	}
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	rows, err := s.pool.Query(ctx, `SELECT r.id, ms.key, r.path, r.kind, r.revision, octet_length(r.body), a.key, r.created_at, r.updated_at
		FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id JOIN seats a ON a.id = r.author_seat_id
		WHERE r.organization_id = $1 AND r.store_id = ANY($2) AND NOT r.archived AND starts_with(r.path, $3)
		ORDER BY `+col+` `+dir+`, ms.key, r.path OFFSET $4 LIMIT $5`, orgID, storeIDs, prefix, offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
}

// SearchHit is a bounded, attributed full-text search result.
type SearchHit struct {
	RecordID  string    `json:"record_id"`
	Store     string    `json:"store"`
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Snippet   string    `json:"snippet"`
	Tags      []string  `json:"tags,omitempty"`
	Revision  int       `json:"revision"`
	Author    string    `json:"author_seat"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Search runs a Postgres full-text query (websearch syntax) over the
// authorised stores, optionally under a path prefix. An empty query lists
// the most recently updated records.
func (s *Store) Search(ctx context.Context, orgID string, storeIDs []string, query, prefix string, tags []string, offset, limit int) ([]SearchHit, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, ms.key, r.path, r.kind,
			CASE WHEN $3 = '' THEN left(r.body, 200)
			     ELSE ts_headline('english', r.body, websearch_to_tsquery('english', $3), 'MaxFragments=2,MaxWords=25,MinWords=8') END,
			r.tags, r.revision, a.key, r.updated_at
		FROM memory_records r JOIN memory_stores ms ON ms.id = r.store_id JOIN seats a ON a.id = r.author_seat_id
		WHERE r.organization_id = $1 AND r.store_id = ANY($2) AND NOT r.archived AND starts_with(r.path, $4)
		  AND ($3 = '' OR r.search @@ websearch_to_tsquery('english', $3))
		  AND (cardinality($5::text[]) = 0 OR r.tags @> $5::text[])
		ORDER BY CASE WHEN $3 = '' THEN 0 ELSE ts_rank(r.search, websearch_to_tsquery('english', $3)) END DESC,
			r.updated_at DESC, r.id
		OFFSET $6 LIMIT $7`, orgID, storeIDs, query, prefix, nonNil(tags), offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SearchHit, error) {
		var h SearchHit
		err := r.Scan(&h.RecordID, &h.Store, &h.Path, &h.Kind, &h.Snippet, &h.Tags, &h.Revision, &h.Author, &h.UpdatedAt)
		return h, err
	})
}

// LiteralCandidates returns live records under prefix whose body contains
// any (all=false) or all (all=true) of the literal substrings, newest first.
// The caller finds the matching lines.
func (s *Store) LiteralCandidates(ctx context.Context, orgID string, storeIDs []string, prefix string, queries []string,
	caseSensitive, all bool, offset, limit int) ([]Record, error) {
	patterns := make([]string, len(queries))
	for i, q := range queries {
		patterns[i] = "%" + likeEscape(q) + "%"
	}
	op, quant := "ILIKE", "ANY"
	if caseSensitive {
		op = "LIKE"
	}
	if all {
		quant = "ALL"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
		WHERE r.organization_id = $1 AND r.store_id = ANY($2) AND NOT r.archived AND starts_with(r.path, $3)
		  AND r.body `+op+` `+quant+`($4)
		ORDER BY r.updated_at DESC, r.id OFFSET $5 LIMIT $6`, orgID, storeIDs, prefix, patterns, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// FileWrite creates or replaces the note at a path.
type FileWrite struct {
	OrganizationID string
	StoreID        string
	AuthorID       string
	Path           string
	Text           string
	Tags           []string
	SourceRefs     []string
	// ExpectedRevision, when set, makes the write a compare-and-swap: 0
	// means the path must not exist yet.
	ExpectedRevision *int
}

// WriteFile creates the note at Path, or replaces its text when it exists.
// Tags and source refs are kept when nil. A structured record at the path
// is never overwritten (ErrStructured).
func (s *Store) WriteFile(ctx context.Context, f Fence, in FileWrite) (rec *Record, created bool, err error) {
	var id string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var cur struct {
			id, kind, author string
			revision         int
		}
		err := tx.QueryRow(ctx, `SELECT r.id, r.kind, r.revision, a.key FROM memory_records r JOIN seats a ON a.id = r.author_seat_id
			WHERE r.organization_id = $1 AND r.store_id = $2 AND r.path = $3 AND NOT r.archived FOR UPDATE OF r`,
			in.OrganizationID, in.StoreID, in.Path).Scan(&cur.id, &cur.kind, &cur.revision, &cur.author)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if in.ExpectedRevision != nil && *in.ExpectedRevision != 0 {
				return &RevisionConflict{}
			}
			id, created = uuid.NewString(), true
			if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, organization_id, store_id, revision, path, kind, body, tags,
					author_seat_id, source_refs) VALUES ($1, $2, $3, 1, $4, 'note', $5, $6, $7, $8)`,
				id, in.OrganizationID, in.StoreID, in.Path, in.Text, nonNil(in.Tags), in.AuthorID, nonNil(in.SourceRefs)); err != nil {
				if isUniqueViolation(err) {
					return &RevisionConflict{}
				}
				return err
			}
		case err != nil:
			return err
		case cur.kind != KindNote:
			return ErrStructured
		case in.ExpectedRevision != nil && *in.ExpectedRevision != cur.revision:
			return &RevisionConflict{Current: cur.revision, CurrentAuthor: cur.author}
		default:
			id = cur.id
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, body = $2,
					tags = COALESCE($3, tags), source_refs = COALESCE($4, source_refs), author_seat_id = $5, updated_at = now()
				WHERE id = $1`, id, in.Text, in.Tags, in.SourceRefs, in.AuthorID); err != nil {
				return err
			}
		}
		return appendRevision(ctx, tx, id, f.Generation)
	})
	if err != nil {
		return nil, false, err
	}
	rec, err = s.Record(ctx, in.OrganizationID, id, []string{in.StoreID})
	return rec, created, err
}

// AppendFile appends text to the note at path, creating it when missing. It
// fails with ErrInvalid when the result would exceed maxBytes.
func (s *Store) AppendFile(ctx context.Context, f Fence, orgID, storeID, authorID, path, text string, maxBytes int) (*Record, error) {
	var id string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var kind string
		var size int
		err := tx.QueryRow(ctx, `SELECT id, kind, octet_length(body) FROM memory_records
			WHERE organization_id = $1 AND store_id = $2 AND path = $3 AND NOT archived FOR UPDATE`,
			orgID, storeID, path).Scan(&id, &kind, &size)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if len(text) > maxBytes {
				return fmt.Errorf("%w: text exceeds %d bytes", ErrInvalid, maxBytes)
			}
			id = uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, organization_id, store_id, revision, path, kind, body, author_seat_id)
				VALUES ($1, $2, $3, 1, $4, 'note', $5, $6)`, id, orgID, storeID, path, text, authorID); err != nil {
				return err
			}
		case err != nil:
			return err
		case kind != KindNote:
			return ErrStructured
		case size+len(text) > maxBytes:
			return fmt.Errorf("%w: the note would exceed %d bytes; continue in a new path", ErrInvalid, maxBytes)
		default:
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, body = body || $2, author_seat_id = $3,
				updated_at = now() WHERE id = $1`, id, text, authorID); err != nil {
				return err
			}
		}
		return appendRevision(ctx, tx, id, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, orgID, id, []string{storeID})
}

// ArchiveRecord hides a note from normal retrieval if it is still at the
// expected revision. Structured records are not archived this way.
func (s *Store) ArchiveRecord(ctx context.Context, f Fence, orgID, recordID, authorID string, expected int, storeIDs []string) (*Record, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		cur, err := lockRecord(ctx, tx, orgID, recordID, storeIDs)
		if err != nil {
			return err
		}
		if cur.kind != KindNote {
			return ErrStructured
		}
		if cur.revision != expected {
			return &RevisionConflict{Current: cur.revision, CurrentAuthor: cur.author}
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, archived = true, author_seat_id = $2,
			updated_at = now() WHERE id = $1`, recordID, authorID); err != nil {
			return err
		}
		return appendRevision(ctx, tx, recordID, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, orgID, recordID, storeIDs)
}

type lockedRecord struct {
	kind, author string
	revision     int
	data         []byte
}

func lockRecord(ctx context.Context, tx pgx.Tx, orgID, recordID string, storeIDs []string) (lockedRecord, error) {
	var cur lockedRecord
	err := tx.QueryRow(ctx, `SELECT r.kind, r.revision, a.key, r.data FROM memory_records r JOIN seats a ON a.id = r.author_seat_id
		WHERE r.organization_id = $1 AND r.id = $2 AND r.store_id = ANY($3) FOR UPDATE OF r`, orgID, recordID, storeIDs).
		Scan(&cur.kind, &cur.revision, &cur.author, &cur.data)
	return cur, notFound(err)
}

// CreateStructured creates a structured record (kind work) at
// "<dir>/<prefix><n>.md", allocating n from the store's sequence. render
// receives n and returns the record's data and rendered body.
func (s *Store) CreateStructured(ctx context.Context, f Fence, orgID, storeID, authorID, kind, dir, prefix string,
	render func(n int64) (data json.RawMessage, body string, err error)) (*Record, error) {
	var id string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRow(ctx, `UPDATE memory_stores SET work_seq = work_seq + 1 WHERE id = $1 RETURNING work_seq`, storeID).
			Scan(&n); err != nil {
			return notFound(err)
		}
		data, body, err := render(n)
		if err != nil {
			return err
		}
		id = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, organization_id, store_id, revision, path, kind, data, body,
			author_seat_id) VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $8)`,
			id, orgID, storeID, fmt.Sprintf("%s/%s%d.md", dir, prefix, n), kind, data, body, authorID); err != nil {
			return err
		}
		return appendRevision(ctx, tx, id, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, orgID, id, []string{storeID})
}

// ReviseStructured changes a structured record of the given kind through
// change, which receives the current data and returns the new data and
// rendered body, or an error to refuse. A stale expected revision is a
// *RevisionConflict; nothing is overwritten.
func (s *Store) ReviseStructured(ctx context.Context, f Fence, orgID, recordID, authorID, kind string, expected int, storeIDs []string,
	change func(data json.RawMessage) (json.RawMessage, string, error)) (*Record, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, ErrNotFound
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		cur, err := lockRecord(ctx, tx, orgID, recordID, storeIDs)
		if err != nil {
			return err
		}
		if cur.kind != kind {
			return ErrNotFound
		}
		if cur.revision != expected {
			return &RevisionConflict{Current: cur.revision, CurrentAuthor: cur.author}
		}
		data, body, err := change(cur.data)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_records SET revision = revision + 1, data = $2, body = $3, author_seat_id = $4,
			updated_at = now() WHERE id = $1`, recordID, data, body, authorID); err != nil {
			return err
		}
		return appendRevision(ctx, tx, recordID, f.Generation)
	})
	if err != nil {
		return nil, err
	}
	return s.Record(ctx, orgID, recordID, storeIDs)
}

// StructuredRecords lists live structured records of kind in the authorised
// stores, optionally only those whose data field equals value, newest first.
func (s *Store) StructuredRecords(ctx context.Context, orgID, kind string, storeIDs []string, field, value string, offset, limit int) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
		WHERE r.organization_id = $1 AND r.kind = $2 AND r.store_id = ANY($3) AND NOT r.archived
		  AND ($4 = '' OR r.data->>$4 = $5)
		ORDER BY r.updated_at DESC, r.id OFFSET $6 LIMIT $7`, orgID, kind, storeIDs, field, value, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// appendRevision copies the record's current state into its append-only
// history with the author's lease generation.
func appendRevision(ctx context.Context, tx pgx.Tx, recordID string, gen int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO memory_revisions (record_id, revision, store_id, path, data, body, tags, author_seat_id,
			author_generation, source_refs, archived)
		SELECT id, revision, store_id, path, data, body, tags, author_seat_id, $2, source_refs, archived FROM memory_records WHERE id = $1`,
		recordID, gen)
	return err
}

// RevisionInfo is one entry of a record's history (body omitted).
type RevisionInfo struct {
	Revision   int       `json:"revision"`
	Path       string    `json:"path"`
	Author     string    `json:"author_seat"`
	Generation *int64    `json:"author_generation,omitempty"`
	SourceRefs []string  `json:"source_refs,omitempty"`
	Archived   bool      `json:"archived,omitempty"`
	BodyBytes  int       `json:"bytes"`
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
	rows, err := s.pool.Query(ctx, `SELECT v.revision, v.path, a.key, v.author_generation, v.source_refs, v.archived,
			octet_length(v.body), v.created_at
		FROM memory_revisions v JOIN seats a ON a.id = v.author_seat_id
		WHERE v.record_id = $1 ORDER BY v.revision DESC OFFSET $2 LIMIT $3`, recordID, offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RevisionInfo, error) {
		var v RevisionInfo
		err := r.Scan(&v.Revision, &v.Path, &v.Author, &v.Generation, &v.SourceRefs, &v.Archived, &v.BodyBytes, &v.CreatedAt)
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
