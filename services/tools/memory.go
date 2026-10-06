package tools

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

// Memory limits.
const (
	maxTitle      = 512
	maxBody       = 256 << 10
	maxTags       = 32
	readChunk     = 12 << 10
	searchDefault = 10
	searchMax     = 25
)

func hasMemory(op string) func(*compile.SeatManifest, *compile.Manifest) bool {
	return func(sm *compile.SeatManifest, _ *compile.Manifest) bool { return policy.HasMemory(sm, op) }
}

func (r *Registry) memoryTools() []*tool {
	return []*tool{
		{name: "memory.stores", description: "List the memory stores this seat can use and the operations allowed on each.",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
				return slices.ContainsFunc(sm.Capabilities, func(c compile.Capability) bool { return strings.HasPrefix(c.Resource, "memory:") })
			},
			handle: r.memoryStores},
		{name: "memory.search", description: "Full-text search (web search syntax) over authorised memory stores. Returns bounded, attributed hits; an empty query lists recent records.",
			schema:  `{"type":"object","properties":{"query":{"type":"string"},"stores":{"type":"array","items":{"type":"string"},"description":"store keys; default all searchable stores"},"tags":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer","minimum":1,"maximum":25},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: hasMemory("search"), handle: r.memorySearch},
		{name: "memory.read", description: "Read a memory record (or a past revision) by id. Long bodies are returned in chunks; pass offset to continue.",
			schema:  `{"type":"object","properties":{"record_id":{"type":"string"},"revision":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"required":["record_id"],"additionalProperties":false}`,
			allowed: hasMemory("read"), handle: r.memoryRead},
		{name: "memory.write", description: "Create a memory record in a store you can write.",
			schema:   `{"type":"object","properties":{"store":{"type":"string"},"title":{"type":"string","maxLength":512},"body":{"type":"string"},"tags":{"type":"array","items":{"type":"string"},"maxItems":32},"source_refs":{"type":"array","items":{"type":"string"}}},"required":["store","title","body"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("write"), handle: r.memoryWrite},
		{name: "memory.revise", description: "Update a record if it is still at expected_revision. On a concurrent change a conflict result reports the current revision; nothing is overwritten.",
			schema:   `{"type":"object","properties":{"record_id":{"type":"string"},"expected_revision":{"type":"integer","minimum":1},"title":{"type":"string","maxLength":512},"body":{"type":"string"},"tags":{"type":"array","items":{"type":"string"},"maxItems":32},"source_refs":{"type":"array","items":{"type":"string"}}},"required":["record_id","expected_revision"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("revise"), handle: r.memoryRevise},
		{name: "memory.archive", description: "Hide a record from normal retrieval while keeping its history (compare-and-swap on expected_revision).",
			schema:   `{"type":"object","properties":{"record_id":{"type":"string"},"expected_revision":{"type":"integer","minimum":1}},"required":["record_id","expected_revision"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("archive"), handle: r.memoryArchive},
		{name: "memory.publish", description: "Create an attributed copy or summary of a record in another store you can write. Provenance is recorded in source_refs.",
			schema:   `{"type":"object","properties":{"record_id":{"type":"string"},"destination":{"type":"string"},"title":{"type":"string","maxLength":512},"body":{"type":"string","description":"summary to publish instead of the full body"}},"required":["record_id","destination"],"additionalProperties":false}`,
			mutating: true,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
				return policy.HasMemory(sm, "publish") && policy.HasMemory(sm, "write")
			},
			handle: r.memoryPublish},
		{name: "memory.history", description: "List a record's revisions with authors and provenance, newest first.",
			schema:  `{"type":"object","properties":{"record_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"cursor":{"type":"string"}},"required":["record_id"],"additionalProperties":false}`,
			allowed: hasMemory("history"), handle: r.memoryHistory},
	}
}

// authorisedStores resolves the active stores on which the seat holds op,
// optionally narrowed to requested keys. It runs before any record query so
// that queries only ever touch authorised stores (A12). A requested store the
// seat cannot use is reported exactly like one that does not exist.
func (r *Registry) authorisedStores(ctx context.Context, c *Call, op string, requested ...string) ([]string, error) {
	keys := policy.MemoryStores(&c.Seat.Manifest, op)
	if len(requested) > 0 {
		for _, k := range requested {
			if !slices.Contains(keys, k) {
				return nil, toolErr("not_found", "memory store %q is not available for %s", k, op)
			}
		}
		keys = requested
	}
	if len(keys) == 0 {
		return nil, nil
	}
	m, err := r.d.Store.ActiveStoreIDs(ctx, c.Seat.OrganizationID, keys)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m))
	for _, k := range keys {
		if id, ok := m[k]; ok {
			ids = append(ids, id)
		} else if len(requested) > 0 {
			return nil, toolErr("not_found", "memory store %q is not available for %s", k, op)
		}
	}
	return ids, nil
}

func (r *Registry) memoryStores(_ context.Context, c *Call) (any, error) {
	var stores []runtimeapi.MemoryStoreInfo
	for _, cap := range c.Seat.Manifest.Capabilities {
		if key, ok := strings.CutPrefix(cap.Resource, "memory:"); ok {
			stores = append(stores, runtimeapi.MemoryStoreInfo{Key: key, Personal: key == c.Seat.Manifest.PersonalMemory, Operations: cap.Operations})
		}
	}
	return map[string]any{"stores": stores}, nil
}

func (r *Registry) memorySearch(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Query  string   `json:"query"`
		Stores []string `json:"stores"`
		Tags   []string `json:"tags"`
		Limit  int      `json:"limit"`
		Cursor string   `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.Limit, searchDefault, searchMax)
	if err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "search", a.Stores...)
	if err != nil {
		return nil, err
	}
	var hits []store.SearchHit
	if len(ids) > 0 {
		if hits, err = r.d.Store.Search(ctx, c.Seat.OrganizationID, ids, strings.TrimSpace(a.Query), a.Tags, offset, limit+1); err != nil {
			return nil, err
		}
	}
	more := len(hits) > limit
	hits = hits[:min(len(hits), limit)]
	return fit(hits, offset, more, func(h []store.SearchHit, next string) any {
		return map[string]any{"results": nonNilSlice(h), "next_cursor": next}
	}), nil
}

func (r *Registry) memoryRead(ctx context.Context, c *Call) (any, error) {
	var a struct {
		RecordID string `json:"record_id"`
		Revision int    `json:"revision"`
		Offset   int    `json:"offset"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "read")
	if err != nil {
		return nil, err
	}
	var rec *store.Record
	if a.Revision > 0 {
		rec, err = r.d.Store.RecordRevision(ctx, c.Seat.OrganizationID, a.RecordID, a.Revision, ids)
	} else {
		rec, err = r.d.Store.Record(ctx, c.Seat.OrganizationID, a.RecordID, ids)
	}
	if err != nil {
		return nil, err
	}
	if a.Offset < 0 || a.Offset > len(rec.Body) {
		return nil, toolErr("invalid", "offset beyond body (%d bytes)", len(rec.Body))
	}
	total := len(rec.Body)
	chunk, more := truncate(rec.Body[a.Offset:], readChunk)
	rec.Body = chunk
	out := map[string]any{"record": rec, "body_offset": a.Offset, "body_bytes": total}
	if more {
		out["next_offset"] = a.Offset + len(chunk)
	}
	return out, nil
}

func validateContent(title, body *string, tags []string) error {
	if title != nil && (strings.TrimSpace(*title) == "" || len(*title) > maxTitle) {
		return toolErr("invalid", "title must be 1-%d bytes", maxTitle)
	}
	if body != nil && len(*body) > maxBody {
		return toolErr("invalid", "body exceeds %d bytes; store large content as several records", maxBody)
	}
	if len(tags) > maxTags {
		return toolErr("invalid", "at most %d tags", maxTags)
	}
	return nil
}

func (r *Registry) memoryWrite(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store      string   `json:"store"`
		Title      string   `json:"title"`
		Body       string   `json:"body"`
		Tags       []string `json:"tags"`
		SourceRefs []string `json:"source_refs"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := validateContent(&a.Title, &a.Body, a.Tags); err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "write", a.Store)
	if err != nil {
		return nil, err
	}
	rec, err := r.d.Store.WriteRecord(ctx, c.fence(), store.NewRecord{OrganizationID: c.Seat.OrganizationID, StoreID: ids[0],
		AuthorID: c.Seat.ID, Title: a.Title, Body: a.Body, Tags: a.Tags, SourceRefs: a.SourceRefs})
	if err != nil {
		return nil, err
	}
	return map[string]any{"record_id": rec.ID, "store": rec.Store, "revision": rec.Revision}, nil
}

func (r *Registry) revise(ctx context.Context, c *Call, op string, ch store.RecordChange) (any, error) {
	ids, err := r.authorisedStores(ctx, c, op)
	if err != nil {
		return nil, err
	}
	ch.OrganizationID, ch.AuthorID = c.Seat.OrganizationID, c.Seat.ID
	rec, err := r.d.Store.ReviseRecord(ctx, c.fence(), ch, ids)
	if _, ok := store.IsConflict(err); ok {
		r.d.Metrics.MemoryConflicts.Inc()
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"record_id": rec.ID, "store": rec.Store, "revision": rec.Revision, "archived": rec.Archived}, nil
}

func (r *Registry) memoryRevise(ctx context.Context, c *Call) (any, error) {
	var a struct {
		RecordID         string   `json:"record_id"`
		ExpectedRevision int      `json:"expected_revision"`
		Title            *string  `json:"title"`
		Body             *string  `json:"body"`
		Tags             []string `json:"tags"`
		SourceRefs       []string `json:"source_refs"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := validateContent(a.Title, a.Body, a.Tags); err != nil {
		return nil, err
	}
	if a.ExpectedRevision < 1 {
		return nil, toolErr("invalid", "expected_revision is required")
	}
	return r.revise(ctx, c, "revise", store.RecordChange{RecordID: a.RecordID, ExpectedRevision: a.ExpectedRevision,
		Title: a.Title, Body: a.Body, Tags: a.Tags, SourceRefs: a.SourceRefs})
}

func (r *Registry) memoryArchive(ctx context.Context, c *Call) (any, error) {
	var a struct {
		RecordID         string `json:"record_id"`
		ExpectedRevision int    `json:"expected_revision"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.ExpectedRevision < 1 {
		return nil, toolErr("invalid", "expected_revision is required")
	}
	return r.revise(ctx, c, "archive", store.RecordChange{RecordID: a.RecordID, ExpectedRevision: a.ExpectedRevision, Archive: true})
}

func (r *Registry) memoryPublish(ctx context.Context, c *Call) (any, error) {
	var a struct {
		RecordID    string  `json:"record_id"`
		Destination string  `json:"destination"`
		Title       *string `json:"title"`
		Body        *string `json:"body"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := validateContent(a.Title, a.Body, nil); err != nil {
		return nil, err
	}
	src, err := r.authorisedStores(ctx, c, "publish")
	if err != nil {
		return nil, err
	}
	rec, err := r.d.Store.Record(ctx, c.Seat.OrganizationID, a.RecordID, src)
	if err != nil {
		return nil, err
	}
	dst, err := r.authorisedStores(ctx, c, "write", a.Destination)
	if err != nil {
		return nil, err
	}
	if dst[0] == rec.StoreID {
		return nil, toolErr("invalid", "destination is the record's own store")
	}
	title, body := rec.Title, rec.Body
	if a.Title != nil {
		title = *a.Title
	}
	if a.Body != nil {
		body = *a.Body
	}
	provenance := append([]string{"memory:" + rec.ID + "@" + strconv.Itoa(rec.Revision) + " (" + rec.Store + ")"}, rec.SourceRefs...)
	out, err := r.d.Store.WriteRecord(ctx, c.fence(), store.NewRecord{OrganizationID: c.Seat.OrganizationID, StoreID: dst[0],
		AuthorID: c.Seat.ID, Title: title, Body: body, Tags: rec.Tags, SourceRefs: provenance})
	if err != nil {
		return nil, err
	}
	return map[string]any{"record_id": out.ID, "store": out.Store, "revision": out.Revision, "source_refs": provenance}, nil
}

func (r *Registry) memoryHistory(ctx context.Context, c *Call) (any, error) {
	var a struct {
		RecordID string `json:"record_id"`
		Limit    int    `json:"limit"`
		Cursor   string `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.Limit, 20, 50)
	if err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "history")
	if err != nil {
		return nil, err
	}
	revs, err := r.d.Store.History(ctx, c.Seat.OrganizationID, a.RecordID, ids, offset, limit+1)
	if err != nil {
		return nil, err
	}
	more := len(revs) > limit
	revs = revs[:min(len(revs), limit)]
	return fit(revs, offset, more, func(v []store.RevisionInfo, next string) any {
		return map[string]any{"record_id": a.RecordID, "revisions": nonNilSlice(v), "next_cursor": next}
	}), nil
}

func nonNilSlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
