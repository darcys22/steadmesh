//go:build integration

package platform_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

func TestNativeTeamWorkReviewAndConcurrentClaim(t *testing.T) {
	e := newEnv(t)
	sp := orgfixture.Spec()
	team := sp.Teams["engineering"]
	team.Coordination = &spec.TeamCoordination{Mode: "self_organising", WorkStore: "engineering", ReviewPolicy: "required", MissionEditors: []string{"seat:lead"}}
	sp.Teams["engineering"] = team
	e.sync(sp)
	lead, eng, rev, outsider := e.seat("lead"), e.seat("engineer"), e.seat("reviewer"), e.seat("rep_b")
	w := lead.mustTool("work.create", map[string]any{"store": "engineering", "objective": "Implement safe peers", "acceptance": []string{"tests pass"}})
	id := w["work_id"].(string)
	var claims atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*seat{eng, rev} {
		wg.Add(1)
		go func(s *seat) {
			defer wg.Done()
			_, bad := s.tool("work.claim", map[string]any{"work_id": id, "expected_revision": 1})
			if !bad {
				claims.Add(1)
			}
		}(s)
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claim winners %d", claims.Load())
	}
	got := lead.mustTool("work.get", map[string]any{"work_id": id})
	owner, reviewer := eng, rev
	if got["owner"] == "reviewer" {
		owner, reviewer = rev, eng
	}
	current := num(got["revision"])
	if _, bad := outsider.tool("work.get", map[string]any{"work_id": id}); !bad {
		t.Fatal("outside team read work")
	}
	if _, bad := owner.tool("work.refine", map[string]any{"work_id": id, "expected_revision": current, "objective": "Different objective", "acceptance": []string{}, "reason": "because"}); !bad {
		t.Fatal("non-editor refined objective")
	}
	updated := owner.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": current, "status": "in_review", "evidence": []string{"commit abc; tests passed"}})
	current = num(updated["revision"])
	if _, bad := owner.tool("work.review", map[string]any{"work_id": id, "expected_revision": current, "approve": true, "note": "looks good"}); !bad {
		t.Fatal("self review accepted")
	}
	if _, bad := owner.tool("work.update", map[string]any{"work_id": id, "expected_revision": current, "status": "done"}); !bad {
		t.Fatal("done without review")
	}
	reviewed := reviewer.mustTool("work.review", map[string]any{"work_id": id, "expected_revision": current, "approve": true, "note": "checked evidence"})
	current = num(reviewed["revision"])
	// Changing evidence must invalidate an approval even in the same done request.
	if _, bad := owner.tool("work.update", map[string]any{"work_id": id, "expected_revision": current, "status": "done", "evidence": []string{"different implementation"}}); !bad {
		t.Fatal("changed deliverable reused review")
	}
	done := owner.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": current, "status": "done"})
	current = num(done["revision"])
	// A completed item cannot silently keep done after its reviewed evidence changes.
	reopened := reviewer.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": current, "evidence": []string{"new finding"}})
	current = num(reopened["revision"])
	if reopened["status"] != "in_review" || reopened["review_current"] != false {
		t.Fatalf("stale completion: %v", reopened)
	}
	approvedAgain := reviewer.mustTool("work.review", map[string]any{"work_id": id, "expected_revision": current, "approve": true, "note": "verified new finding"})
	current = num(approvedAgain["revision"])
	done = owner.mustTool("work.update", map[string]any{"work_id": id, "expected_revision": current, "status": "done"})
	current = num(done["revision"])
	replanned := owner.mustTool("work.update_plan", map[string]any{"work_id": id, "expected_revision": current, "plan": []map[string]any{{"step": "additional test", "status": "pending"}}})
	current = num(replanned["revision"])
	if replanned["status"] != "in_review" || replanned["review_current"] != false {
		t.Fatalf("plan kept done: %v", replanned)
	}

	refined := lead.mustTool("work.refine", map[string]any{"work_id": id, "expected_revision": current, "objective": "Include failure recovery", "acceptance": []string{"recovery test"}, "reason": "new requirement"})
	if refined["status"] != "ready" || refined["review_current"] != false {
		t.Fatalf("refinement did not reopen: %v", refined)
	}
}
