//go:build integration

package platform_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func num(v any) int { f, _ := v.(float64); return int(f) }

func TestWorkItemsEnforceOwnershipAndRules(t *testing.T) {
	e := newEnv(t)
	sp := orgfixture.Spec()
	team := sp.Teams["engineering"]
	team.SharedMemory = map[string][]string{"engineering": {"read", "search", "write", "revise", "archive", "history"}}
	sp.Teams["engineering"] = team
	e.sync(sp)
	lead, eng, rev := e.seat("lead"), e.seat("engineer"), e.seat("reviewer")

	w := lead.mustTool("work.create", map[string]any{"store": "engineering", "objective": "Build the login page",
		"acceptance": []string{"tests pass"}})
	id := w["work_id"].(string)
	if id != "engineering/W-1" || w["status"] != "ready" || w["owner"] != "" {
		t.Fatalf("created %v", w)
	}

	// The engineer claims it; the reviewer's concurrent claim at the same
	// revision is an explicit conflict naming the owner, not an overwrite.
	claimed := eng.mustTool("work.claim", map[string]any{"work_id": id, "expected_revision": 1})
	if claimed["owner"] != "engineer" || claimed["status"] != "in_progress" {
		t.Fatalf("claim %v", claimed)
	}
	out, isErr := rev.tool("work.claim", map[string]any{"work_id": id, "expected_revision": 1})
	if !isErr || out["error"] != "conflict" || out["details"].(map[string]any)["owner"] != "engineer" {
		t.Fatalf("stale claim: %v", out)
	}
	// Even at the current revision, someone else's item cannot be claimed.
	cur := num(claimed["revision"])
	if out, isErr := rev.tool("work.claim", map[string]any{"work_id": id, "expected_revision": cur}); !isErr || out["error"] != "conflict" {
		t.Fatalf("claim of owned item: %v", out)
	}

	// Plans: one step in progress at most, and only the owner changes them.
	if out, isErr := eng.tool("work.update_plan", map[string]any{"work_id": id, "expected_revision": cur,
		"plan": []map[string]any{{"step": "a", "status": "in_progress"}, {"step": "b", "status": "in_progress"}}}); !isErr || out["error"] != "invalid" {
		t.Fatalf("two steps in progress: %v", out)
	}
	if out, isErr := rev.tool("work.update_plan", map[string]any{"work_id": id, "expected_revision": cur,
		"plan": []map[string]any{{"step": "a", "status": "pending"}}}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("non-owner plan: %v", out)
	}
	planned := eng.mustTool("work.update_plan", map[string]any{"work_id": id, "expected_revision": cur,
		"plan": []map[string]any{{"step": "write form", "status": "in_progress"}, {"step": "write tests", "status": "pending"}}})
	// update_plan is revision-checked too.
	if out, isErr := eng.tool("work.update_plan", map[string]any{"work_id": id, "expected_revision": cur,
		"plan": []map[string]any{{"step": "x", "status": "pending"}}}); !isErr || out["error"] != "conflict" {
		t.Fatalf("stale plan update: %v", out)
	}
	cur = num(planned["revision"])

	// Anyone who can write may add notes; status belongs to the owner.
	noted := rev.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": cur, "note": "remember the a11y checklist"})
	cur = num(noted["revision"])
	if out, isErr := rev.tool("work.update", map[string]any{"work_id": id, "expected_revision": cur, "status": "done"}); !isErr || out["error"] != "forbidden" {
		t.Fatalf("non-owner status change: %v", out)
	}
	if out, isErr := eng.tool("work.update", map[string]any{"work_id": id, "expected_revision": cur, "status": "done"}); !isErr || out["error"] != "invalid" {
		t.Fatalf("done without evidence: %v", out)
	}
	if out, isErr := eng.tool("work.update", map[string]any{"work_id": id, "expected_revision": cur, "status": "blocked"}); !isErr || out["error"] != "invalid" {
		t.Fatalf("blocked without blocker: %v", out)
	}
	done := eng.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": cur, "status": "done",
		"evidence": []string{"commit abc123", "tests: 42 passed"}})
	if done["status"] != "done" || len(done["evidence"].([]any)) != 2 {
		t.Fatalf("done %v", done)
	}

	// Plain memory writes cannot touch a work item.
	if out, isErr := eng.tool("memory.write", map[string]any{"store": "engineering", "path": "work/W-1.md", "text": "owner: me"}); !isErr || out["error"] != "invalid" {
		t.Fatalf("memory.write under work/: %v", out)
	}
	if out, isErr := eng.tool("memory.archive", map[string]any{"store": "engineering", "path": "work/W-1.md", "expected_revision": num(done["revision"])}); !isErr || out["error"] != "invalid" {
		t.Fatalf("memory.archive of a work item: %v", out)
	}
	// It is still readable and searchable like any record.
	read := eng.mustTool("memory.read", map[string]any{"store": "engineering", "path": "work/W-1.md"})
	if read["kind"] != "work" || !strings.Contains(read["text"].(string), "status: done") {
		t.Fatalf("read work item: %v", read)
	}
	list := lead.mustTool("work.list", map[string]any{"owner": "engineer"})
	if items := list["work"].([]any); len(items) != 1 || items[0].(map[string]any)["status"] != "done" {
		t.Fatalf("work.list %v", list)
	}
}

func TestFileStyleMemory(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	lead, eng := e.seat("lead"), e.seat("engineer")
	text := "line one\nthe deploy target is prod\nline three\nrollback with care\nline five\n"
	lead.mustTool("memory.write", map[string]any{"store": "engineering", "path": "notes/deploy.md", "text": text})
	lead.mustTool("memory.write", map[string]any{"store": "engineering", "path": "decisions/auth.md", "text": "use OIDC\n"})

	// Line ranges, with negatives counting from the end.
	got := eng.mustTool("memory.read", map[string]any{"store": "engineering", "path": "notes/deploy.md", "start_line": 2, "stop_line": -2})
	if got["text"] != "the deploy target is prod\nline three\nrollback with care\n" || num(got["total_lines"]) != 5 {
		t.Fatalf("line range: %q", got["text"])
	}
	// Prefix listing.
	ls := eng.mustTool("memory.list", map[string]any{"store": "engineering", "prefix": "notes/"})
	if recs := ls["records"].([]any); len(recs) != 1 || recs[0].(map[string]any)["path"] != "notes/deploy.md" {
		t.Fatalf("list: %v", ls)
	}
	// Literal search with context, and all_on_same_line.
	hits := eng.mustTool("memory.search", map[string]any{"queries": []string{"DEPLOY"}, "context_lines": 1})
	files := hits["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("literal search: %v", hits)
	}
	m := files[0].(map[string]any)["matches"].([]any)[0].(map[string]any)
	if num(m["line"]) != 2 || m["before"].([]any)[0] != "line one" || m["after"].([]any)[0] != "line three" {
		t.Fatalf("match: %v", m)
	}
	hits = eng.mustTool("memory.search", map[string]any{"queries": []string{"deploy", "rollback"}, "match_mode": map[string]any{"type": "all_on_same_line"}})
	if len(hits["files"].([]any)) != 0 {
		t.Fatalf("all_on_same_line matched across lines: %v", hits)
	}
	hits = eng.mustTool("memory.search", map[string]any{"queries": []string{"deploy", "rollback"},
		"match_mode": map[string]any{"type": "all_within_lines", "line_count": 3}})
	if len(hits["files"].([]any)) != 1 {
		t.Fatalf("all_within_lines: %v", hits)
	}
	// Appends never overwrite each other.
	eng.mustTool("memory.append", map[string]any{"store": "engineering", "path": "notes/log.md", "text": "engineer: started\n"})
	lead.mustTool("memory.append", map[string]any{"store": "engineering", "path": "notes/log.md", "text": "lead: reviewed\n"})
	log := eng.mustTool("memory.read", map[string]any{"store": "engineering", "path": "notes/log.md"})
	if log["text"] != "engineer: started\nlead: reviewed\n" || num(log["revision"]) != 2 {
		t.Fatalf("appends: %v", log)
	}
	// Bad paths are rejected.
	for _, p := range []string{"/abs.md", "a/../b.md", "dir/", "work/W-9.md"} {
		if out, isErr := eng.tool("memory.write", map[string]any{"store": "engineering", "path": p, "text": "x"}); !isErr || out["error"] != "invalid" {
			t.Errorf("path %q: %v", p, out)
		}
	}
}

func TestPassiveMessagesDoNotWakeAndArriveWithNextTurn(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	repA, lead := e.seat("rep_a"), e.seat("lead")

	sent := repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "FYI: the demo moved to Friday", "wake": false})
	if sent["recipient_pending"] != nil && num(sent["recipient_pending"]) != 0 {
		t.Fatalf("a passive message counts as pending: %v", sent)
	}
	// Nothing to run: the lead is not woken.
	if d := lead.next(); d != nil {
		t.Fatalf("passive message started a turn: %+v", d)
	}
	code, b := e.request("GET", runtimeapi.PathInternalOrgs+e.org+"/runtime", controllerToken, nil, nil)
	var rt runtimeapi.RuntimeResponse
	_ = json.Unmarshal(b, &rt)
	if code != http.StatusOK || rt.Seats["lead"].PendingDeliveries != 0 {
		t.Fatalf("runtime reports passive as pending: %d %+v", code, rt.Seats["lead"])
	}
	// A real message starts a turn and carries the queued one with it.
	repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "please prepare the demo"})
	d := lead.next()
	if d == nil || d.Message.Body != "please prepare the demo" || len(d.Passive) != 1 || d.Passive[0].Body != "FYI: the demo moved to Friday" {
		t.Fatalf("delivery = %+v", d)
	}
	lead.ack(d)
	// Handed over once.
	repA.mustTool("messages.send", map[string]any{"to": "lead", "body": "another"})
	if d := lead.next(); d == nil || len(d.Passive) != 0 {
		t.Fatalf("passive delivered twice: %+v", d)
	}
}

func TestRecoveryListsOwnedWork(t *testing.T) {
	e := newEnv(t)
	e.sync(orgfixture.Spec())
	lead, eng := e.seat("lead"), e.seat("engineer")
	w := lead.mustTool("work.create", map[string]any{"store": "engineering", "objective": "Migrate the database", "owner": "engineer"})
	eng.mustTool("work.update_plan", map[string]any{"work_id": w["work_id"], "expected_revision": num(w["revision"]),
		"plan": []map[string]any{{"step": "write migration", "status": "in_progress"}}})
	code, b := eng.do("GET", runtimeapi.PathBootstrap, nil)
	var boot runtimeapi.Bootstrap
	_ = json.Unmarshal(b, &boot)
	if code != http.StatusOK || len(boot.Recovery.Work) != 1 || boot.Recovery.Work[0].WorkID != "engineering/W-1" ||
		boot.Recovery.Work[0].CurrentStep != "write migration" {
		t.Fatalf("recovery = %d %+v", code, boot.Recovery)
	}
}
