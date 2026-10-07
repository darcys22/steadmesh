package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

// Work items are optional helpers for coordination that benefits from
// explicit ownership: a shared objective, an owner, a status, a plan and
// evidence, stored as structured records under work/ in a shared memory
// store. Agents can coordinate with plain notes and messages alone; nothing
// requires a work item.
//
// The rules are enforced here, and the store refuses any other write to a
// work item: claiming only succeeds when the item is unowned, status and
// plan change only by the owner (or the creator while it is unowned), done
// requires evidence, blocked requires the blocking condition, and every
// change is revision-checked so a concurrent change is a conflict, never a
// silent overwrite.

// Work statuses.
const (
	WorkReady      = "ready"
	WorkInProgress = "in_progress"
	WorkInReview   = "in_review"
	WorkBlocked    = "blocked"
	WorkDone       = "done"
	WorkCancelled  = "cancelled"
)

var workStatuses = []string{WorkReady, WorkInProgress, WorkInReview, WorkBlocked, WorkDone, WorkCancelled}

// Plan step statuses (at most one in_progress).
const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepCompleted  = "completed"
)

const (
	maxWorkLog   = 50
	maxWorkText  = 4 << 10
	maxWorkItems = 50
)

var workIDRe = regexp.MustCompile(`^([a-z][a-z0-9_]*)/W-([0-9]+)$`)

type planStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type workLog struct {
	At   time.Time `json:"at"`
	Seat string    `json:"seat"`
	Text string    `json:"text"`
}

// workItem is the structured data of a work record.
type workItem struct {
	ID         string     `json:"id"`
	Objective  string     `json:"objective"`
	Creator    string     `json:"creator"`
	Owner      string     `json:"owner,omitempty"`
	Status     string     `json:"status"`
	Blocker    string     `json:"blocker,omitempty"`
	Plan       []planStep `json:"plan,omitempty"`
	DependsOn  []string   `json:"depends_on,omitempty"`
	Acceptance []string   `json:"acceptance,omitempty"`
	Evidence   []string   `json:"evidence,omitempty"`
	Links      []string   `json:"links,omitempty"`
	Parent     string     `json:"parent,omitempty"`
	Log        []workLog  `json:"log,omitempty"`
}

func (w *workItem) note(seat, text string) {
	if text == "" {
		return
	}
	w.Log = append(w.Log, workLog{At: time.Now().UTC(), Seat: seat, Text: text})
	if len(w.Log) > maxWorkLog {
		w.Log = w.Log[len(w.Log)-maxWorkLog:]
	}
}

// render produces the record's text: what memory.read and memory.search see.
func (w *workItem) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", w.ID, w.Objective)
	fmt.Fprintf(&b, "status: %s\n", w.Status)
	if w.Owner != "" {
		fmt.Fprintf(&b, "owner: %s\n", w.Owner)
	} else {
		b.WriteString("owner: (unclaimed)\n")
	}
	fmt.Fprintf(&b, "created by: %s\n", w.Creator)
	if w.Blocker != "" {
		fmt.Fprintf(&b, "blocked by: %s\n", w.Blocker)
	}
	if w.Parent != "" {
		fmt.Fprintf(&b, "parent: %s\n", w.Parent)
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	list("Acceptance", w.Acceptance)
	list("Depends on", w.DependsOn)
	if len(w.Plan) > 0 {
		b.WriteString("\n## Plan\n")
		for _, s := range w.Plan {
			mark := map[string]string{StepPending: "[ ]", StepInProgress: "[>]", StepCompleted: "[x]"}[s.Status]
			fmt.Fprintf(&b, "- %s %s\n", mark, s.Step)
		}
	}
	list("Evidence", w.Evidence)
	list("Links", w.Links)
	if len(w.Log) > 0 {
		b.WriteString("\n## Log\n")
		for _, l := range w.Log {
			fmt.Fprintf(&b, "- %s %s: %s\n", l.At.Format(time.RFC3339), l.Seat, l.Text)
		}
	}
	return b.String()
}

func (w *workItem) encode() (json.RawMessage, string, error) {
	b, err := json.Marshal(w)
	return b, w.render(), err
}

func workAllowed(op string) func(*compile.SeatManifest, *compile.Manifest) bool {
	return func(sm *compile.SeatManifest, org *compile.Manifest) bool {
		return slices.ContainsFunc(policy.MemoryStores(sm, op), func(k string) bool { return k != sm.PersonalMemory })
	}
}

const workIDProp = `"work_id":{"type":"string","description":"<store>/W-<n>"}`

func (r *Registry) workTools() []*tool {
	return []*tool{
		{name: "work.create", description: "Optionally record a shared work item in a team store: an objective with acceptance criteria, dependencies and an optional owner. Use it when explicit ownership helps (several people, long-running work); plain notes and messages are fine otherwise.",
			schema: `{"type":"object","properties":{"store":{"type":"string"},"objective":{"type":"string","minLength":1},` +
				`"acceptance":{"type":"array","items":{"type":"string"}},"depends_on":{"type":"array","items":{"type":"string"}},` +
				`"links":{"type":"array","items":{"type":"string"},"description":"message, record or artifact ids"},"parent":{"type":"string"},` +
				`"owner":{"type":"string","description":"seat key to assign; omit to leave unclaimed"}},"required":["store","objective"],"additionalProperties":false}`,
			mutating: true, allowed: workAllowed("write"), handle: r.workCreate},
		{name: "work.get", description: "Read a work item: its status, owner, plan, evidence and log, with the revision to pass to changes.",
			schema:  `{"type":"object","properties":{` + workIDProp + `},"required":["work_id"],"additionalProperties":false}`,
			allowed: workAllowed("read"), handle: r.workGet},
		{name: "work.list", description: "List work items in the stores you can read, optionally by owner (a seat key, or \"me\") and status.",
			schema: `{"type":"object","properties":{"store":{"type":"string"},"owner":{"type":"string"},"status":{"enum":["ready","in_progress","in_review","blocked","done","cancelled"]},` +
				`"max_results":{"type":"integer","minimum":1,"maximum":50},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: workAllowed("read"), handle: r.workList},
		{name: "work.claim", description: "Take ownership of an unclaimed work item. If someone else owns it, the result is a conflict naming the current owner; nothing changes. Resolve it with them rather than retrying.",
			schema:   `{"type":"object","properties":{` + workIDProp + `,"expected_revision":{"type":"integer","minimum":1},"note":{"type":"string"}},"required":["work_id","expected_revision"],"additionalProperties":false}`,
			mutating: true, allowed: workAllowed("revise"), handle: r.workClaim},
		{name: "work.release", description: "Give up ownership of a work item you own, with a note for whoever picks it up.",
			schema:   `{"type":"object","properties":{` + workIDProp + `,"expected_revision":{"type":"integer","minimum":1},"note":{"type":"string","minLength":1}},"required":["work_id","expected_revision","note"],"additionalProperties":false}`,
			mutating: true, allowed: workAllowed("revise"), handle: r.workRelease},
		{name: "work.update_plan", description: "Replace a work item's plan: a list of steps with status pending, in_progress or completed. At most one step can be in_progress.",
			schema: `{"type":"object","properties":{` + workIDProp + `,"expected_revision":{"type":"integer","minimum":1},"explanation":{"type":"string"},` +
				`"plan":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string","minLength":1},"status":{"enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},` +
				`"required":["work_id","expected_revision","plan"],"additionalProperties":false}`,
			mutating: true, allowed: workAllowed("revise"), handle: r.workUpdatePlan},
		{name: "work.update", description: "Record progress on a work item: a note, evidence (links, commit ids, test output), and optionally a new status. " +
			"Set done only when the acceptance criteria are actually met and evidence is attached. Set blocked only when you cannot make progress without someone else, and say what would unblock it. " +
			"Anyone who can write the store may add notes and evidence; the owner (or the creator while unclaimed) changes status.",
			schema: `{"type":"object","properties":{` + workIDProp + `,"expected_revision":{"type":"integer","minimum":1},` +
				`"status":{"enum":["ready","in_progress","in_review","blocked","done","cancelled"]},"note":{"type":"string"},` +
				`"evidence":{"type":"array","items":{"type":"string"}},"blocker":{"type":"string","description":"what would unblock it; required for blocked"}},` +
				`"required":["work_id","expected_revision"],"additionalProperties":false}`,
			mutating: true, allowed: workAllowed("revise"), handle: r.workUpdate},
	}
}

// workRef resolves a work id to its record in a store where the seat holds op.
func (r *Registry) workRef(ctx context.Context, c *Call, op, id string) (*store.Record, []string, error) {
	m := workIDRe.FindStringSubmatch(id)
	if m == nil {
		return nil, nil, toolErr("invalid", "work_id must look like <store>/W-<n>")
	}
	ids, err := r.authorisedStores(ctx, c, op, m[1])
	if err != nil {
		return nil, nil, err
	}
	rec, err := r.d.Store.RecordByPath(ctx, c.Seat.OrganizationID, ids[0], workDir+"W-"+m[2]+".md")
	if err != nil {
		return nil, nil, err
	}
	if rec.Kind != store.KindWork {
		return nil, nil, toolErr("not_found", "%s is not a work item", id)
	}
	return rec, ids, nil
}

func decodeWork(rec *store.Record) (*workItem, error) {
	w := &workItem{}
	if err := json.Unmarshal(rec.Data, w); err != nil {
		return nil, fmt.Errorf("work item %s: %w", rec.Path, err)
	}
	return w, nil
}

func workView(rec *store.Record, w *workItem) map[string]any {
	return map[string]any{"work_id": w.ID, "revision": rec.Revision, "objective": w.Objective, "status": w.Status,
		"owner": w.Owner, "creator": w.Creator, "blocker": w.Blocker, "plan": nonNilSlice(w.Plan),
		"acceptance": nonNilSlice(w.Acceptance), "depends_on": nonNilSlice(w.DependsOn), "evidence": nonNilSlice(w.Evidence),
		"links": nonNilSlice(w.Links), "parent": w.Parent, "log": nonNilSlice(w.Log), "path": rec.Path, "updated_at": rec.UpdatedAt}
}

func checkWorkText(fields ...string) error {
	for _, f := range fields {
		if len(f) > maxWorkText {
			return toolErr("invalid", "text fields are limited to %d bytes; put long content in a note and link it", maxWorkText)
		}
	}
	return nil
}

func (r *Registry) workCreate(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store      string   `json:"store"`
		Objective  string   `json:"objective"`
		Acceptance []string `json:"acceptance"`
		DependsOn  []string `json:"depends_on"`
		Links      []string `json:"links"`
		Parent     string   `json:"parent"`
		Owner      string   `json:"owner"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Objective) == "" {
		return nil, toolErr("invalid", "objective is required")
	}
	if err := checkWorkText(append(append([]string{a.Objective}, a.Acceptance...), a.DependsOn...)...); err != nil {
		return nil, err
	}
	if a.Store == c.Seat.Manifest.PersonalMemory {
		return nil, toolErr("invalid", "work items live in shared stores, not your personal store")
	}
	ids, err := r.authorisedStores(ctx, c, "write", a.Store)
	if err != nil {
		return nil, err
	}
	var item *workItem
	rec, err := r.d.Store.CreateStructured(ctx, c.fence(), c.Seat.OrganizationID, ids[0], c.Seat.ID, store.KindWork, "work", "W-",
		func(n int64) (json.RawMessage, string, error) {
			item = &workItem{ID: a.Store + "/W-" + strconv.FormatInt(n, 10), Objective: a.Objective, Creator: c.Seat.Key,
				Owner: a.Owner, Status: WorkReady, Acceptance: a.Acceptance, DependsOn: a.DependsOn, Links: a.Links, Parent: a.Parent}
			if a.Owner != "" {
				item.note(c.Seat.Key, "created and assigned to "+a.Owner)
			} else {
				item.note(c.Seat.Key, "created")
			}
			return item.encode()
		})
	if err != nil {
		return nil, err
	}
	return workView(rec, item), nil
}

func (r *Registry) workGet(ctx context.Context, c *Call) (any, error) {
	var a struct {
		WorkID string `json:"work_id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	rec, _, err := r.workRef(ctx, c, "read", a.WorkID)
	if err != nil {
		return nil, err
	}
	w, err := decodeWork(rec)
	if err != nil {
		return nil, err
	}
	return workView(rec, w), nil
}

func (r *Registry) workList(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Store      string `json:"store"`
		Owner      string `json:"owner"`
		Status     string `json:"status"`
		MaxResults int    `json:"max_results"`
		Cursor     string `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.MaxResults, 20, maxWorkItems)
	if err != nil {
		return nil, err
	}
	ids, err := r.authorisedStores(ctx, c, "read", a.Store)
	if err != nil {
		return nil, err
	}
	owner := a.Owner
	if owner == "me" {
		owner = c.Seat.Key
	}
	field, value := "", ""
	switch {
	case owner != "":
		field, value = "owner", owner
	case a.Status != "":
		field, value = "status", a.Status
	}
	type summary struct {
		WorkID    string    `json:"work_id"`
		Revision  int       `json:"revision"`
		Objective string    `json:"objective"`
		Status    string    `json:"status"`
		Owner     string    `json:"owner,omitempty"`
		Blocker   string    `json:"blocker,omitempty"`
		Step      string    `json:"current_step,omitempty"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	var out []summary
	more := false
	if len(ids) > 0 {
		recs, err := r.d.Store.StructuredRecords(ctx, c.Seat.OrganizationID, store.KindWork, ids, field, value, offset, limit+1)
		if err != nil {
			return nil, err
		}
		more = len(recs) > limit
		for _, rec := range recs[:min(len(recs), limit)] {
			w, err := decodeWork(&rec)
			if err != nil {
				return nil, err
			}
			if owner != "" && a.Status != "" && w.Status != a.Status {
				continue
			}
			s := summary{WorkID: w.ID, Revision: rec.Revision, Objective: w.Objective, Status: w.Status, Owner: w.Owner,
				Blocker: w.Blocker, UpdatedAt: rec.UpdatedAt}
			for _, st := range w.Plan {
				if st.Status == StepInProgress {
					s.Step = st.Step
				}
			}
			out = append(out, s)
		}
	}
	return fit(out, offset, more, func(s []summary, next string) any {
		return map[string]any{"work": nonNilSlice(s), "next_cursor": next}
	}), nil
}

// mutateWork applies change to a work item under its revision check.
func (r *Registry) mutateWork(ctx context.Context, c *Call, id string, expected int, change func(w *workItem) error) (any, error) {
	rec, ids, err := r.workRef(ctx, c, "revise", id)
	if err != nil {
		return nil, err
	}
	var item *workItem
	out, err := r.d.Store.ReviseStructured(ctx, c.fence(), c.Seat.OrganizationID, rec.ID, c.Seat.ID, store.KindWork, expected, ids,
		func(data json.RawMessage) (json.RawMessage, string, error) {
			item = &workItem{}
			if err := json.Unmarshal(data, item); err != nil {
				return nil, "", err
			}
			if err := change(item); err != nil {
				return nil, "", err
			}
			return item.encode()
		})
	if conflict, ok := store.IsConflict(err); ok {
		r.d.Metrics.MemoryConflicts.Inc()
		cur, _, _ := r.workRef(ctx, c, "read", id)
		details := map[string]any{"current_revision": conflict.Current, "current_author": conflict.CurrentAuthor}
		if cur != nil {
			if w, err := decodeWork(cur); err == nil {
				details["owner"], details["status"] = w.Owner, w.Status
			}
		}
		return nil, &Error{Code: "conflict", Message: "the work item changed since the expected revision; read it with work.get and decide from its current state",
			Details: details}
	}
	var te *Error
	if errors.As(err, &te) {
		return nil, te
	}
	if err != nil {
		return nil, err
	}
	return workView(out, item), nil
}

// mayDirect reports whether the seat may change status and plan: the owner,
// or the creator while nobody owns the item.
func mayDirect(w *workItem, seat string) bool {
	return w.Owner == seat || (w.Owner == "" && w.Creator == seat)
}

func (r *Registry) workClaim(ctx context.Context, c *Call) (any, error) {
	var a struct {
		WorkID           string `json:"work_id"`
		ExpectedRevision int    `json:"expected_revision"`
		Note             string `json:"note"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	me := c.Seat.Key
	return r.mutateWork(ctx, c, a.WorkID, a.ExpectedRevision, func(w *workItem) error {
		switch {
		case w.Owner == me:
			return nil
		case w.Owner != "":
			return &Error{Code: "conflict", Message: fmt.Sprintf("%s is owned by %s; ask them, or the creator %s, rather than claiming it", w.ID, w.Owner, w.Creator),
				Details: map[string]any{"owner": w.Owner, "status": w.Status}}
		case w.Status == WorkDone || w.Status == WorkCancelled:
			return toolErr("invalid", "%s is %s", w.ID, w.Status)
		}
		w.Owner = me
		if w.Status == WorkReady {
			w.Status = WorkInProgress
		}
		w.note(me, strings.TrimSpace("claimed. "+a.Note))
		return nil
	})
}

func (r *Registry) workRelease(ctx context.Context, c *Call) (any, error) {
	var a struct {
		WorkID           string `json:"work_id"`
		ExpectedRevision int    `json:"expected_revision"`
		Note             string `json:"note"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	me := c.Seat.Key
	return r.mutateWork(ctx, c, a.WorkID, a.ExpectedRevision, func(w *workItem) error {
		if w.Owner != me {
			return toolErr("forbidden", "only the owner (%s) can release %s", or(w.Owner, "nobody"), w.ID)
		}
		w.Owner = ""
		if w.Status != WorkDone && w.Status != WorkCancelled {
			w.Status, w.Blocker = WorkReady, ""
		}
		w.note(me, "released: "+a.Note)
		return nil
	})
}

func (r *Registry) workUpdatePlan(ctx context.Context, c *Call) (any, error) {
	var a struct {
		WorkID           string     `json:"work_id"`
		ExpectedRevision int        `json:"expected_revision"`
		Explanation      string     `json:"explanation"`
		Plan             []planStep `json:"plan"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	inProgress := 0
	for _, s := range a.Plan {
		if !slices.Contains([]string{StepPending, StepInProgress, StepCompleted}, s.Status) || strings.TrimSpace(s.Step) == "" {
			return nil, toolErr("invalid", "each step needs text and a status of pending, in_progress or completed")
		}
		if s.Status == StepInProgress {
			inProgress++
		}
		if err := checkWorkText(s.Step); err != nil {
			return nil, err
		}
	}
	if inProgress > 1 {
		return nil, toolErr("invalid", "at most one step can be in_progress")
	}
	me := c.Seat.Key
	return r.mutateWork(ctx, c, a.WorkID, a.ExpectedRevision, func(w *workItem) error {
		if !mayDirect(w, me) {
			return toolErr("forbidden", "only the owner (%s) can change the plan of %s; send them a message", or(w.Owner, w.Creator), w.ID)
		}
		w.Plan = a.Plan
		w.note(me, strings.TrimSpace("plan updated. "+a.Explanation))
		return nil
	})
}

func (r *Registry) workUpdate(ctx context.Context, c *Call) (any, error) {
	var a struct {
		WorkID           string   `json:"work_id"`
		ExpectedRevision int      `json:"expected_revision"`
		Status           string   `json:"status"`
		Note             string   `json:"note"`
		Evidence         []string `json:"evidence"`
		Blocker          string   `json:"blocker"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.Status != "" && !slices.Contains(workStatuses, a.Status) {
		return nil, toolErr("invalid", "unknown status %q", a.Status)
	}
	if a.Status == "" && a.Note == "" && len(a.Evidence) == 0 {
		return nil, toolErr("invalid", "nothing to update: give a status, note or evidence")
	}
	if err := checkWorkText(append([]string{a.Note, a.Blocker}, a.Evidence...)...); err != nil {
		return nil, err
	}
	me := c.Seat.Key
	return r.mutateWork(ctx, c, a.WorkID, a.ExpectedRevision, func(w *workItem) error {
		w.Evidence = append(w.Evidence, a.Evidence...)
		if a.Status != "" && a.Status != w.Status {
			if !mayDirect(w, me) {
				return toolErr("forbidden", "only the owner (%s) can change the status of %s; add a note or message them",
					or(w.Owner, w.Creator), w.ID)
			}
			terminal := w.Status == WorkDone || w.Status == WorkCancelled
			switch {
			case terminal && a.Status != WorkReady:
				return toolErr("invalid", "%s is %s; set it back to ready to reopen it", w.ID, w.Status)
			case a.Status == WorkDone && len(w.Evidence) == 0:
				return toolErr("invalid", "done needs evidence that the acceptance criteria are met")
			case a.Status == WorkBlocked && strings.TrimSpace(a.Blocker) == "":
				return toolErr("invalid", "blocked needs the blocker: what would unblock it")
			}
			w.Status = a.Status
			w.Blocker = ""
			if a.Status == WorkBlocked {
				w.Blocker = a.Blocker
			}
			w.note(me, "status: "+a.Status)
		}
		w.note(me, a.Note)
		if len(a.Evidence) > 0 {
			w.note(me, "evidence added: "+strings.Join(a.Evidence, "; "))
		}
		return nil
	})
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// OwnedWork lists the unfinished work items a seat owns in the stores it can
// read, for its recovery context.
func OwnedWork(ctx context.Context, st *store.Store, seat *store.Seat) ([]runtimeapi.WorkSummary, error) {
	keys := slices.DeleteFunc(policy.MemoryStores(&seat.Manifest, "read"), func(k string) bool { return k == seat.Manifest.PersonalMemory })
	if len(keys) == 0 {
		return nil, nil
	}
	m, err := st.ActiveStoreIDs(ctx, seat.OrganizationID, keys)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m))
	for _, id := range m {
		ids = append(ids, id)
	}
	recs, err := st.StructuredRecords(ctx, seat.OrganizationID, store.KindWork, ids, "owner", seat.Key, 0, maxWorkItems)
	if err != nil {
		return nil, err
	}
	var out []runtimeapi.WorkSummary
	for _, rec := range recs {
		w, err := decodeWork(&rec)
		if err != nil || w.Status == WorkDone || w.Status == WorkCancelled {
			continue
		}
		v := runtimeapi.WorkSummary{WorkID: w.ID, Objective: w.Objective, Status: w.Status, Revision: rec.Revision}
		for _, s := range w.Plan {
			if s.Status == StepInProgress {
				v.CurrentStep = s.Step
			}
		}
		if n := len(w.Log); n > 0 {
			v.LastNote = w.Log[n-1].Text
		}
		out = append(out, v)
	}
	return out, nil
}
