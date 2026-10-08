//go:build e2e

package e2e

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var workIDRe = regexp.MustCompile(`engineering/W-[0-9]+`)

// delegateChain sends a task from Sean's representative to the lead, whose
// script may delegate further; the end result is forwarded back to Sean.
func (f *fakes) delegateChain(t *testing.T, script, want string, timeout time.Duration) string {
	t.Helper()
	f.dm(t, userSean, "/delegate eng_lead /task\n"+script, "")
	return f.waitPosted(t, "D0SEAN", want, timeout)
}

// askRep asks Sean's representative to run tool calls itself and report.
func (f *fakes) askRep(t *testing.T, script, want string) string {
	t.Helper()
	f.dm(t, userSean, script+"\n/report", "")
	return f.waitPosted(t, "D0SEAN", want, 2*time.Minute)
}

// coordinationWithoutLinear runs with no Linear connection declared: seats
// coordinate through messages and shared memory alone.
func coordinationWithoutLinear(t *testing.T, f *fakes) {
	step(t, "coordination with no Linear and no work items", func(t *testing.T) {
		if out := mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "agentorganization", orgName,
			"-o", "jsonpath={.spec.connections}"); strings.Contains(out, "linear") {
			t.Fatalf("Linear is declared: %s", out)
		}
		// The lead writes the plan to the team notes and hands the work to
		// the engineer, who builds and asks the reviewer, who approves. Each
		// appends to the shared note; the end result comes back to Sean.
		script := `/tool memory.write {"store":"engineering","path":"notes/login.md","text":"# Login page\nplan: engineer builds, reviewer reviews\n"}
/delegate engineer /task
/tool memory.append {"store":"engineering","path":"notes/login.md","text":"engineer: implemented the login page\n"}
/delegate reviewer /task
/tool memory.append {"store":"engineering","path":"notes/login.md","text":"reviewer: approved\n"}
/reply reviewed and approved`
		f.delegateChain(t, script, "Update from reviewer: reviewed and approved", 5*time.Minute)
		// The representative reads the team's shared record to report.
		note := f.askRep(t, `/tool memory.read {"store":"engineering","path":"notes/login.md"}`, "notes/login.md")
		for _, want := range []string{"plan: engineer builds", "engineer: implemented", "reviewer: approved"} {
			if !strings.Contains(note, want) {
				t.Fatalf("shared note lacks %q:\n%s", want, note)
			}
		}
	})

	var workID string
	step(t, "work items: claim, conflict and recovery after a restart", func(t *testing.T) {
		created := f.delegateChain(t, `/tool work.create {"store":"engineering","objective":"Build the signup page","acceptance":["tests pass"]}`,
			"Build the signup page", 3*time.Minute)
		workID = workIDRe.FindString(created)
		if workID == "" {
			t.Fatalf("no work id in:\n%s", created)
		}
		claimed := f.delegateChain(t, `/delegate engineer /task
/tool work.claim {"work_id":"`+workID+`","expected_revision":1,"note":"on it"}
/tool work.update_plan {"work_id":"`+workID+`","expected_revision":2,"plan":[{"step":"write the form","status":"in_progress"}]}`,
			"tool work.update_plan", 4*time.Minute)
		if !strings.Contains(claimed, `"owner":"engineer"`) {
			t.Fatalf("claim did not take ownership:\n%s", claimed)
		}
		// A concurrent claim at the old revision is an explicit conflict
		// naming the owner, not an overwrite.
		conflict := f.delegateChain(t, `/tool work.claim {"work_id":"`+workID+`","expected_revision":1}`, "tool work.claim error", 3*time.Minute)
		if !strings.Contains(conflict, `"conflict"`) || !strings.Contains(conflict, "engineer") {
			t.Fatalf("expected a conflict naming the owner:\n%s", conflict)
		}

		// A message queued for the engineer without waking it, then a crash.
		f.delegateChain(t, `/tool messages.send {"to":"engineer","body":"FYI: use the shared form styles","wake":false}`,
			"tool messages.send", 3*time.Minute)
		sts := seatStatefulSet(t, "engineer")
		genBefore, _ := strconv.Atoi(jsonpath(t, "agentseat", sts, "{.status.leaseGeneration}"))
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "delete", "pod", sts+"-0", "--ignore-not-found", "--wait=true", "--timeout=120s")

		// After the restart the engineer finds its work, its progress and the
		// queued message.
		recovered := f.delegateChain(t, `/delegate engineer /task
/tool work.list {"owner":"me"}`, "tool work.list", 5*time.Minute)
		for _, want := range []string{workID, `"status":"in_progress"`, "write the form", "queued message from eng_lead: FYI: use the shared form styles"} {
			if !strings.Contains(recovered, want) {
				t.Fatalf("after restart the engineer lacks %q:\n%s", want, recovered)
			}
		}
		genAfter, _ := strconv.Atoi(jsonpath(t, "agentseat", sts, "{.status.leaseGeneration}"))
		if genAfter <= genBefore {
			t.Fatalf("lease generation %d -> %d: the Pod was not replaced", genBefore, genAfter)
		}
	})
}

// enableLinear declares the Linear connection with work publication, checks
// publication and direct use, and that a Linear outage never blocks the
// organisation.
func enableLinear(t *testing.T, f *fakes, env []string) {
	withLinear := append(append([]string{}, env...), "TF_VAR_enable_linear=true", "TF_VAR_publish_work_to_tracker=true")
	issues := func(t *testing.T, title string) []string {
		var st struct {
			Issues []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"issues"`
		}
		f.get(t, f.linear+"/_test/state", &st)
		var ids []string
		for _, is := range st.Issues {
			if strings.HasPrefix(is.Title, title) {
				ids = append(ids, is.ID)
			}
		}
		return ids
	}

	step(t, "enable Linear: existing work is published once", func(t *testing.T) {
		mustRun(t, withLinear, "make", "-C", "examples", "apply-organisation")
		waitFor(t, "the signup work item to appear in Linear", 3*time.Minute, func() bool {
			return len(issues(t, "engineering/W-1: Build the signup page")) == 1
		})
	})

	step(t, "agents read human input from Linear directly", func(t *testing.T) {
		id := issues(t, "engineering/W-1: ")[0]
		f.post(t, f.linear+"/_test/comment", map[string]any{"issue_id": id, "author": "sean", "body": "Please add SSO too"})
		out := f.delegateChain(t, `/tool connections.invoke {"connection":"linear","operation":"comment.read","params":{"issue_id":"`+id+`"}}`,
			"tool connections.invoke", 3*time.Minute)
		if !strings.Contains(out, "Please add SSO too") {
			t.Fatalf("comment not read:\n%s", out)
		}
	})

	step(t, "a Linear outage never blocks the organisation", func(t *testing.T) {
		f.post(t, f.linear+"/_test/keys", map[string]any{"valid": []string{"nobody-has-this-key"}})
		// Verification passes: Linear is optional, so only IntegrationsDegraded reports it.
		if code := runCode(t, nil, filepath.Join(root, "bin/orgctl"), "verify", "--context", kctx, "--namespace", orgNS, "--timeout", "4m"); code != 0 {
			t.Fatal("orgctl verify failed while only the optional Linear connection was down")
		}
		waitFor(t, "IntegrationsDegraded to report Linear", 2*time.Minute, func() bool {
			return jsonpath(t, "agentorganization", orgName, `{.status.conditions[?(@.type=="IntegrationsDegraded")].status}`) == "True"
		})
		// Work carries on internally.
		out := f.delegateChain(t, `/tool work.create {"store":"engineering","objective":"Write the release notes"}`, "Write the release notes", 3*time.Minute)
		id := workIDRe.FindString(out)
		if id == "" || !strings.Contains(out, `"status":"ready"`) {
			t.Fatalf("work item not created while Linear is down:\n%s", out)
		}
		if n := len(issues(t, id+": ")); n != 0 {
			t.Fatalf("published while Linear rejects every key: %d", n)
		}
		// Reconnect: publication converges without duplicates.
		f.post(t, f.linear+"/_test/keys", map[string]any{"valid": []string{}})
		waitFor(t, "the release notes item to be published after reconnecting", 6*time.Minute, func() bool {
			return len(issues(t, id+": Write the release notes")) == 1
		})
		if n := len(issues(t, "engineering/W-1: ")); n != 1 {
			t.Fatalf("signup item published %d times, want 1", n)
		}
		waitFor(t, "IntegrationsDegraded to clear", 6*time.Minute, func() bool {
			_ = runCode(t, nil, filepath.Join(root, "bin/orgctl"), "verify", "--context", kctx, "--namespace", orgNS, "--timeout", "2m")
			return jsonpath(t, "agentorganization", orgName, `{.status.conditions[?(@.type=="IntegrationsDegraded")].status}`) == "False"
		})
	})
}
