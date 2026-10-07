package tools

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

// Memory is addressed like files: each store holds records at paths
// ("notes/plan.md"). Notes are free-form; structured work items live under
// work/ and change only through the work tools.

// Memory limits.
const (
	maxPath       = 512
	maxBody       = 256 << 10
	maxTags       = 32
	readChunk     = 12 << 10
	searchDefault = 10
	searchMax     = 25
	listDefault   = 50
	listMax       = 100
	maxContext    = 5
	maxFileHits   = 5
	// workDir is reserved for structured work items.
	workDir = "work/"
)

func hasMemory(op string) func(*compile.SeatManifest, *compile.Manifest) bool {
	return func(sm *compile.SeatManifest, _ *compile.Manifest) bool { return policy.HasMemory(sm, op) }
}

const (
	locatorProps = `"store":{"type":"string","description":"store key; with path"},"path":{"type":"string"},"record_id":{"type":"string","description":"alternative to store+path"}`
	matchMode    = `"match_mode":{"description":"how literal queries combine: any (default), all_on_same_line, or all_within_lines","oneOf":[` +
		`{"type":"object","properties":{"type":{"enum":["any"]}},"required":["type"]},` +
		`{"type":"object","properties":{"type":{"enum":["all_on_same_line"]}},"required":["type"]},` +
		`{"type":"object","properties":{"type":{"enum":["all_within_lines"]},"line_count":{"type":"integer","minimum":1,"maximum":50}},"required":["type","line_count"]}]}`
)

func (r *Registry) memoryTools() []*tool {
	return []*tool{
		{name: "memory.stores", description: "List the memory stores this seat can use and the operations allowed on each.",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
				return slices.ContainsFunc(sm.Capabilities, func(c compile.Capability) bool { return strings.HasPrefix(c.Resource, "memory:") })
			},
			handle: r.memoryStores},
		{name: "memory.list", description: "List records by path prefix across the stores you can read. Returns paths, kinds, sizes, revisions and authors, not contents.",
			schema: `{"type":"object","properties":{"store":{"type":"string"},"prefix":{"type":"string","description":"path prefix, e.g. notes/ or work/"},` +
				`"order_by":{"enum":["name","created_at","updated_at"]},"order":{"enum":["ascending","descending"]},` +
				`"max_results":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: hasMemory("read"), handle: r.memoryList},
		{name: "memory.read", description: "Read a record by store and path (or record_id), optionally a line range. start_line and stop_line are 1-based and inclusive; negative values count back from the last line. Long reads are returned in parts: pass next_start_line to continue.",
			schema: `{"type":"object","properties":{` + locatorProps + `,"revision":{"type":"integer","minimum":1},` +
				`"start_line":{"type":"integer"},"stop_line":{"type":"integer"}},"additionalProperties":false}`,
			allowed: hasMemory("read"), handle: r.memoryRead},
		{name: "memory.search", description: "Search the stores you can search. Use query for full-text search (web search syntax), or queries for literal substrings matched line by line with optional context lines. Narrow with stores, path_prefix and tags.",
			schema: `{"type":"object","properties":{"query":{"type":"string"},"queries":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":8},` +
				matchMode + `,"case_sensitive":{"type":"boolean"},"context_lines":{"type":"integer","minimum":0,"maximum":5},` +
				`"stores":{"type":"array","items":{"type":"string"}},"path_prefix":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},` +
				`"max_results":{"type":"integer","minimum":1,"maximum":25},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: hasMemory("search"), handle: r.memorySearch},
		{name: "memory.write", description: "Create a note at a path, or replace its text. With expected_revision the write only happens if the note is still at that revision (0: it must not exist yet); otherwise a conflict reports the current revision and nothing is overwritten. Replacing needs the revise permission. Work items (work/) change only through the work tools.",
			schema: `{"type":"object","properties":{"store":{"type":"string"},"path":{"type":"string","maxLength":512},"text":{"type":"string"},` +
				`"tags":{"type":"array","items":{"type":"string"},"maxItems":32},"source_refs":{"type":"array","items":{"type":"string"}},` +
				`"expected_revision":{"type":"integer","minimum":0}},"required":["store","path","text"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("write"), handle: r.memoryWrite},
		{name: "memory.append", description: "Append text to a note, creating it if it does not exist. Concurrent appends never overwrite each other; use it for logs and running notes.",
			schema:   `{"type":"object","properties":{"store":{"type":"string"},"path":{"type":"string","maxLength":512},"text":{"type":"string","minLength":1}},"required":["store","path","text"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("write"), handle: r.memoryAppend},
		{name: "memory.archive", description: "Hide a note from listings and search while keeping its history (only if it is still at expected_revision).",
			schema:   `{"type":"object","properties":{` + locatorProps + `,"expected_revision":{"type":"integer","minimum":1}},"required":["expected_revision"],"additionalProperties":false}`,
			mutating: true, allowed: hasMemory("archive"), handle: r.memoryArchive},
		{name: "memory.publish", description: "Create an attributed copy or summary of a record in another store you can write, at path (default: the same path). Provenance is recorded in source_refs.",
			schema: `{"type":"object","properties":{` + locatorProps + `,"destination":{"type":"string"},"destination_path":{"type":"string"},` +
				`"text":{"type":"string","description":"summary to publish instead of the full text"}},"required":["destination"],"additionalProperties":false}`,
			mutating: true,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
				return policy.HasMemory(sm, "publish") && policy.HasMemory(sm, "write")
			},
			handle: r.memoryPublish},
		{name: "memory.history", description: "List a record's revisions with authors and provenance, newest first.",
			schema:  `{"type":"object","properties":{` + locatorProps + `,"max_results":{"type":"integer","minimum":1,"maximum":50},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: hasMemory("history"), handle: r.memoryHistory},
	}
}

// authorisedStores resolves the active stores on which the seat holds op,
// optionally narrowed to requested keys. It runs before any record query so
// that queries only ever touch authorised stores (A12). A requested store the
// seat cannot use is reported exactly like one that does not exist.
func (r *Registry) authorisedStores(ctx context.Context, c *Call, op string, requested ...string) ([]string, error) {
	keys := policy.MemoryStores(&c.Seat.Manifest, op)
	requested = slices.DeleteFunc(slices.Clone(requested), func(k string) bool { return k == "" })
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

// validPath checks a memory path: relative, slash-separated, no empty, "."
// or ".." segments, no control characters.
func validPath(p string) error {
	if p == "" || len(p) > maxPath {
		return toolErr("invalid", "path must be 1-%d bytes", maxPath)
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return toolErr("invalid", "path %q must be relative and name a record, e.g. notes/plan.md", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return toolErr("invalid", "path %q has an empty, \".\" or \"..\" segment", p)
		}
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return toolErr("invalid", "path contains control characters")
	}
	return nil
}

func notePath(p string) error {
	if err := validPath(p); err != nil {
		return err
	}
	if strings.HasPrefix(p, workDir) {
		return toolErr("invalid", "paths under %s are work items; use the work tools", workDir)
	}
	return nil
}

// locate resolves a record by record_id, or by store and path, in the
// stores on which the seat holds op.
func (r *Registry) locate(ctx context.Context, c *Call, op, storeKey, path, recordID string) (*store.Record, []string, error) {
	switch {
	case recordID != "":
		ids, err := r.authorisedStores(ctx, c, op)
		if err != nil {
			return nil, nil, err
		}
		rec, err := r.d.Store.Record(ctx, c.Seat.OrganizationID, recordID, ids)
		return rec, ids, err
	case storeKey != "" && path != "":
		ids, err := r.authorisedStores(ctx, c, op, storeKey)
		if err != nil {
			return nil, nil, err
		}
		rec, err := r.d.Store.RecordByPath(ctx, c.Seat.OrganizationID, ids[0], path)
		return rec, ids, err
	}
	return nil, nil, toolErr("invalid", "give store and path, or record_id")
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

func (r *Registry) memoryList(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store      string `json:"store"`
		Prefix     string `json:"prefix"`
		OrderBy    string `json:"order_by"`
		Order      string `json:"order"`
		MaxResults int    `json:"max_results"`
		Cursor     string `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.MaxResults, listDefault, listMax)
	if err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "read", a.Store)
	if err != nil {
		return nil, err
	}
	var entries []store.Entry
	if len(ids) > 0 {
		if entries, err = r.d.Store.List(ctx, c.Seat.OrganizationID, ids, a.Prefix, store.ListOrder(a.OrderBy),
			a.Order == "descending", offset, limit+1); err != nil {
			return nil, err
		}
	}
	more := len(entries) > limit
	entries = entries[:min(len(entries), limit)]
	return fit(entries, offset, more, func(e []store.Entry, next string) any {
		return map[string]any{"records": nonNilSlice(e), "next_cursor": next}
	}), nil
}

func (r *Registry) memoryRead(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store     string `json:"store"`
		Path      string `json:"path"`
		RecordID  string `json:"record_id"`
		Revision  int    `json:"revision"`
		StartLine *int   `json:"start_line"`
		StopLine  *int   `json:"stop_line"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	rec, ids, err := r.locate(ctx, c, "read", a.Store, a.Path, a.RecordID)
	if err != nil {
		return nil, err
	}
	if a.Revision > 0 && a.Revision != rec.Revision {
		if rec, err = r.d.Store.RecordRevision(ctx, c.Seat.OrganizationID, rec.ID, a.Revision, ids); err != nil {
			return nil, err
		}
	}
	lines := splitLines(rec.Text)
	total := len(lines)
	start, stop, err := lineRange(a.StartLine, a.StopLine, total)
	if err != nil {
		return nil, err
	}
	// Return whole lines up to the read chunk.
	var b strings.Builder
	last := start - 1
	for i := start; i <= stop; i++ {
		line := lines[i-1]
		if b.Len()+len(line) > readChunk && i > start {
			break
		}
		if b.Len()+len(line) > readChunk {
			line, _ = truncate(line, readChunk)
		}
		b.WriteString(line)
		last = i
	}
	out := map[string]any{"record_id": rec.ID, "store": rec.Store, "path": rec.Path, "kind": rec.Kind, "revision": rec.Revision,
		"total_lines": total, "start_line": start, "stop_line": last, "text": b.String(), "author_seat": rec.Author,
		"updated_at": rec.UpdatedAt}
	if len(rec.Tags) > 0 {
		out["tags"] = rec.Tags
	}
	if len(rec.SourceRefs) > 0 {
		out["source_refs"] = rec.SourceRefs
	}
	if last < stop {
		out["next_start_line"] = last + 1
	}
	return out, nil
}

// splitLines splits text into lines that keep their newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineRange resolves 1-based inclusive bounds, negatives counting back from
// the last line.
func lineRange(start, stop *int, total int) (int, int, error) {
	resolve := func(p *int, def int) int {
		if p == nil {
			return def
		}
		if *p < 0 {
			return total + *p + 1
		}
		return *p
	}
	s, e := resolve(start, 1), resolve(stop, total)
	if total == 0 {
		return 1, 0, nil
	}
	if s < 1 || s > total || e < s || e > total {
		return 0, 0, toolErr("invalid", "line range %d-%d is outside 1-%d", s, e, total)
	}
	return s, e, nil
}

type lineMatch struct {
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

type fileMatches struct {
	RecordID string      `json:"record_id"`
	Store    string      `json:"store"`
	Path     string      `json:"path"`
	Kind     string      `json:"kind"`
	Revision int         `json:"revision"`
	Matches  []lineMatch `json:"matches"`
	More     bool        `json:"more_matches,omitempty"`
}

func (r *Registry) memorySearch(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Query     string   `json:"query"`
		Queries   []string `json:"queries"`
		MatchMode *struct {
			Type      string `json:"type"`
			LineCount int    `json:"line_count"`
		} `json:"match_mode"`
		CaseSensitive bool     `json:"case_sensitive"`
		ContextLines  int      `json:"context_lines"`
		Stores        []string `json:"stores"`
		PathPrefix    string   `json:"path_prefix"`
		Tags          []string `json:"tags"`
		MaxResults    int      `json:"max_results"`
		Cursor        string   `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.MaxResults, searchDefault, searchMax)
	if err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "search", a.Stores...)
	if err != nil {
		return nil, err
	}
	if len(a.Queries) == 0 {
		var hits []store.SearchHit
		if len(ids) > 0 {
			if hits, err = r.d.Store.Search(ctx, c.Seat.OrganizationID, ids, strings.TrimSpace(a.Query), a.PathPrefix, a.Tags,
				offset, limit+1); err != nil {
				return nil, err
			}
		}
		more := len(hits) > limit
		hits = hits[:min(len(hits), limit)]
		return fit(hits, offset, more, func(h []store.SearchHit, next string) any {
			return map[string]any{"results": nonNilSlice(h), "next_cursor": next}
		}), nil
	}
	if a.Query != "" {
		return nil, toolErr("invalid", "give query or queries, not both")
	}
	mode, window := "any", 1
	if a.MatchMode != nil {
		mode = a.MatchMode.Type
		if mode == "all_within_lines" {
			window = a.MatchMode.LineCount
			if window < 1 {
				return nil, toolErr("invalid", "all_within_lines needs line_count >= 1")
			}
		} else if mode != "any" && mode != "all_on_same_line" {
			return nil, toolErr("invalid", "unknown match_mode %q", mode)
		}
	}
	ctxLines := min(max(a.ContextLines, 0), maxContext)
	var files []fileMatches
	if len(ids) > 0 {
		recs, err := r.d.Store.LiteralCandidates(ctx, c.Seat.OrganizationID, ids, a.PathPrefix, a.Queries, a.CaseSensitive,
			mode != "any", offset, limit+1)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			m, more := matchLines(rec.Text, a.Queries, a.CaseSensitive, mode, window, ctxLines)
			if len(m) == 0 {
				continue
			}
			files = append(files, fileMatches{RecordID: rec.ID, Store: rec.Store, Path: rec.Path, Kind: rec.Kind,
				Revision: rec.Revision, Matches: m, More: more})
		}
		more := len(recs) > limit
		if more {
			files = files[:min(len(files), limit)]
		}
		return fit(files, offset, more, func(f []fileMatches, next string) any {
			return map[string]any{"files": nonNilSlice(f), "next_cursor": next}
		}), nil
	}
	return map[string]any{"files": []fileMatches{}, "next_cursor": ""}, nil
}

// matchLines finds the lines matching the literal queries. In
// all_within_lines mode the reported line starts a window of n lines that
// together contain every query.
func matchLines(text string, queries []string, caseSensitive bool, mode string, window, ctxLines int) ([]lineMatch, bool) {
	lines := splitLines(text)
	norm := func(s string) string {
		if caseSensitive {
			return s
		}
		return strings.ToLower(s)
	}
	qs := make([]string, len(queries))
	for i, q := range queries {
		qs[i] = norm(q)
	}
	contains := func(from, to int) (any, all bool) {
		all = true
		for _, q := range qs {
			found := false
			for i := from; i < to && i < len(lines); i++ {
				if strings.Contains(norm(lines[i]), q) {
					found = true
					break
				}
			}
			any = any || found
			all = all && found
		}
		return any, all
	}
	var out []lineMatch
	more := false
	for i := range lines {
		var ok bool
		switch mode {
		case "any":
			ok, _ = contains(i, i+1)
		case "all_on_same_line":
			_, ok = contains(i, i+1)
		default:
			_, ok = contains(i, i+window)
			// Report a window only where its first line holds a query, so
			// one cluster is not reported once per line.
			if ok {
				ok, _ = contains(i, i+1)
			}
		}
		if !ok {
			continue
		}
		if len(out) == maxFileHits {
			more = true
			break
		}
		m := lineMatch{Line: i + 1, Text: clip(lines[i])}
		for j := max(0, i-ctxLines); j < i; j++ {
			m.Before = append(m.Before, clip(lines[j]))
		}
		for j := i + 1; j <= min(len(lines)-1, i+ctxLines); j++ {
			m.After = append(m.After, clip(lines[j]))
		}
		out = append(out, m)
	}
	return out, more
}

func clip(line string) string {
	line = strings.TrimSuffix(line, "\n")
	s, cut := truncate(line, 400)
	if cut {
		s += "…"
	}
	return s
}

func validateNote(text string, tags []string) error {
	if len(text) > maxBody {
		return toolErr("invalid", "text exceeds %d bytes; split it across several paths", maxBody)
	}
	if len(tags) > maxTags {
		return toolErr("invalid", "at most %d tags", maxTags)
	}
	return nil
}

func (r *Registry) memoryWrite(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store            string   `json:"store"`
		Path             string   `json:"path"`
		Text             string   `json:"text"`
		Tags             []string `json:"tags"`
		SourceRefs       []string `json:"source_refs"`
		ExpectedRevision *int     `json:"expected_revision"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := notePath(a.Path); err != nil {
		return nil, err
	}
	if err := validateNote(a.Text, a.Tags); err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "write", a.Store)
	if err != nil {
		return nil, err
	}
	expected := a.ExpectedRevision
	if !slices.Contains(policy.MemoryStores(&c.Seat.Manifest, "revise"), a.Store) && expected == nil {
		// Without revise this seat may only create.
		zero := 0
		expected = &zero
	}
	rec, created, err := r.d.Store.WriteFile(ctx, c.fence(), store.FileWrite{OrganizationID: c.Seat.OrganizationID, StoreID: ids[0],
		AuthorID: c.Seat.ID, Path: a.Path, Text: a.Text, Tags: a.Tags, SourceRefs: a.SourceRefs, ExpectedRevision: expected})
	if err != nil {
		return nil, r.memoryError(err, a.Path)
	}
	return map[string]any{"record_id": rec.ID, "store": rec.Store, "path": rec.Path, "revision": rec.Revision, "created": created}, nil
}

func (r *Registry) memoryError(err error, path string) error {
	if _, ok := store.IsConflict(err); ok {
		r.d.Metrics.MemoryConflicts.Inc()
	}
	if errors.Is(err, store.ErrStructured) {
		return toolErr("invalid", "%s is a work item; change it with the work tools", path)
	}
	return err
}

func (r *Registry) memoryAppend(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store string `json:"store"`
		Path  string `json:"path"`
		Text  string `json:"text"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := notePath(a.Path); err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "write", a.Store)
	if err != nil {
		return nil, err
	}
	rec, err := r.d.Store.AppendFile(ctx, c.fence(), c.Seat.OrganizationID, ids[0], c.Seat.ID, a.Path, a.Text, maxBody)
	if err != nil {
		return nil, r.memoryError(err, a.Path)
	}
	return map[string]any{"record_id": rec.ID, "store": rec.Store, "path": rec.Path, "revision": rec.Revision, "bytes": len(rec.Text)}, nil
}

func (r *Registry) memoryArchive(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store            string `json:"store"`
		Path             string `json:"path"`
		RecordID         string `json:"record_id"`
		ExpectedRevision int    `json:"expected_revision"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	rec, ids, err := r.locate(ctx, c, "archive", a.Store, a.Path, a.RecordID)
	if err != nil {
		return nil, err
	}
	out, err := r.d.Store.ArchiveRecord(ctx, c.fence(), c.Seat.OrganizationID, rec.ID, c.Seat.ID, a.ExpectedRevision, ids)
	if err != nil {
		return nil, r.memoryError(err, rec.Path)
	}
	return map[string]any{"record_id": out.ID, "store": out.Store, "path": out.Path, "revision": out.Revision, "archived": true}, nil
}

func (r *Registry) memoryPublish(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store           string  `json:"store"`
		Path            string  `json:"path"`
		RecordID        string  `json:"record_id"`
		Destination     string  `json:"destination"`
		DestinationPath string  `json:"destination_path"`
		Text            *string `json:"text"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	rec, _, err := r.locate(ctx, c, "publish", a.Store, a.Path, a.RecordID)
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
	path := a.DestinationPath
	if path == "" {
		path = strings.TrimPrefix(rec.Path, workDir)
	}
	if err := notePath(path); err != nil {
		return nil, err
	}
	text := rec.Text
	if a.Text != nil {
		text = *a.Text
	}
	if err := validateNote(text, nil); err != nil {
		return nil, err
	}
	provenance := append([]string{"memory:" + rec.ID + "@" + strconv.Itoa(rec.Revision) + " (" + rec.Store + ":" + rec.Path + ")"}, rec.SourceRefs...)
	zero := 0
	out, _, err := r.d.Store.WriteFile(ctx, c.fence(), store.FileWrite{OrganizationID: c.Seat.OrganizationID, StoreID: dst[0],
		AuthorID: c.Seat.ID, Path: path, Text: text, Tags: rec.Tags, SourceRefs: provenance, ExpectedRevision: &zero})
	if _, ok := store.IsConflict(err); ok {
		return nil, toolErr("conflict", "%s already exists in %s; choose destination_path", path, a.Destination)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"record_id": out.ID, "store": out.Store, "path": out.Path, "revision": out.Revision, "source_refs": provenance}, nil
}

func (r *Registry) memoryHistory(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store      string `json:"store"`
		Path       string `json:"path"`
		RecordID   string `json:"record_id"`
		MaxResults int    `json:"max_results"`
		Cursor     string `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.MaxResults, 20, 50)
	if err != nil {
		return nil, err
	}
	rec, ids, err := r.locate(ctx, c, "history", a.Store, a.Path, a.RecordID)
	if err != nil {
		return nil, err
	}
	revs, err := r.d.Store.History(ctx, c.Seat.OrganizationID, rec.ID, ids, offset, limit+1)
	if err != nil {
		return nil, err
	}
	more := len(revs) > limit
	revs = revs[:min(len(revs), limit)]
	return fit(revs, offset, more, func(v []store.RevisionInfo, next string) any {
		return map[string]any{"record_id": rec.ID, "path": rec.Path, "revisions": nonNilSlice(v), "next_cursor": next}
	}), nil
}

func nonNilSlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
