//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// consoleAPI reads the platform's console API as the console identity.
func consoleAPI(t *testing.T) func(t *testing.T, path string, out any) {
	t.Helper()
	token := strings.TrimSpace(mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "create", "token", "steadmesh-console"))
	platform := portForward(t, "svc/steadmesh-platform", 8080)
	return func(t *testing.T, path string, out any) {
		t.Helper()
		req, _ := http.NewRequest("GET", platform+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, res.StatusCode, b)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

// systemMessages returns the bodies of platform notices delivered to a seat.
func systemMessages(t *testing.T, get func(*testing.T, string, any), orgID, seatKey string) []string {
	t.Helper()
	var list runtimeapi.ConsoleConversationList
	get(t, runtimeapi.PathConsoleOrgs+"/"+orgID+"/conversations?system=true&limit=500", &list)
	var out []string
	for _, c := range list.Conversations {
		if c.Kind != "system" || len(c.Participants) != 1 || c.Participants[0] != seatKey {
			continue
		}
		var d runtimeapi.ConsoleConversationDetail
		get(t, runtimeapi.PathConsoleConversations+c.ID, &d)
		for _, m := range d.Messages {
			out = append(out, m.Body)
		}
	}
	return out
}

func anyContains(list []string, subs ...string) bool {
	for _, s := range list {
		ok := true
		for _, sub := range subs {
			ok = ok && strings.Contains(s, sub)
		}
		if ok {
			return true
		}
	}
	return false
}

// seatRetirement removes a seat from the declaration while it is in the
// middle of a task (the acceptance test for graceful retirement): the
// running turn finishes and its result arrives, the seat saves a handoff on
// its retirement notice, its work item is released, a message queued for it
// goes back to the sender, the representatives get a summary, and its
// runtime is removed with the workspace kept.
func seatRetirement(t *testing.T, f *fakes, env []string) {
	withContractor := append(append([]string{}, env...),
		`TF_VAR_extra_engineering_seats={"contractor":{"role":"engineer","display_name":"Contractor"}}`)
	var workID, seatID, orgID string

	step(t, "A28 add a seat and give it work", func(t *testing.T) {
		mustRun(t, withContractor, "make", "-C", "examples", "apply-organisation")
		sts := seatStatefulSet(t, "contractor")
		seatID = jsonpath(t, "agentseat", sts, "{.spec.seatID}")
		orgID = mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "agentorganization", orgName, "-o", "jsonpath={.status.organizationID}")
		out := f.delegateChain(t, `/tool work.create {"store":"engineering","objective":"Write the quarterly report","owner":"contractor"}`,
			"tool work.create", 4*time.Minute)
		workID = workIDRe.FindString(out)
		if workID == "" || !strings.Contains(out, `"owner":"contractor"`) {
			t.Fatalf("work item not created for the contractor:\n%s", out)
		}
	})

	step(t, "A28 removing a busy seat lets it finish and hand over", func(t *testing.T) {
		get := consoleAPI(t)
		sts := seatStatefulSet(t, "contractor")
		// A long task: the contractor is mid-turn when it is removed.
		f.dm(t, userSean, "/delegate eng_lead /task\n/delegate contractor /task\n/sleep 150s\n"+
			`/tool memory.write {"store":"engineering","path":"notes/report.md","text":"quarterly report drafted"}`, "")
		waitFor(t, "the contractor to be executing", 3*time.Minute, func() bool {
			return jsonpath(t, "agentseat", sts, "{.status.executionState}") == "Executing"
		})
		// A second task queues behind it.
		f.dm(t, userSean, "/delegate eng_lead /task\n/delegate contractor /task\n"+
			`/tool memory.write {"store":"engineering","path":"notes/second.md","text":"never written"}`, "")
		waitFor(t, "the second task to queue for the contractor", 3*time.Minute, func() bool {
			var seats runtimeapi.ConsoleSeatList
			get(t, runtimeapi.PathConsoleOrgs+"/"+orgID+"/seats", &seats)
			for _, s := range seats.Seats {
				if s.SeatKey == "contractor" {
					return s.PendingDeliveries > 0
				}
			}
			return false
		})

		mustRun(t, env, "make", "-C", "examples", "apply-organisation")
		waitFor(t, "the contractor to report Retiring with a deadline", time.Minute, func() bool {
			return jsonpath(t, "agentseat", sts, "{.status.executionState}") == "Retiring" &&
				jsonpath(t, "agentseat", sts, "{.status.retiringUntil}") != ""
		})

		// The running turn finished: its result came back through the lead.
		msg := f.waitPosted(t, "D0SEAN", "Update from contractor", 5*time.Minute)
		if !strings.Contains(msg, "slept 2m30s") || !strings.Contains(msg, "tool memory.write") {
			t.Fatalf("the contractor's last task did not complete:\n%s", msg)
		}
		// After its retirement notice turn the seat retires and its runtime goes.
		waitFor(t, "the contractor AgentSeat to be removed", 5*time.Minute, func() bool {
			_, err := run(nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "agentseat", sts)
			return err != nil
		})
		if out, err := run(nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "statefulset", sts); err == nil {
			t.Fatalf("the retired seat's StatefulSet remains:\n%s", out)
		}
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get", "pvc", names.WorkspaceClaim(sts))

		// Its history is kept: both turns completed and the handoff is saved.
		var d runtimeapi.ConsoleSeatDetail
		get(t, runtimeapi.PathConsoleSeats+seatID, &d)
		if d.Config.RetiredAt == nil || d.Handoff == nil || !strings.Contains(d.Handoff.Objective, "Hand over contractor") {
			t.Fatalf("retired seat: retired_at %v handoff %+v", d.Config.RetiredAt, d.Handoff)
		}
		var task, notice bool
		for _, e := range d.Executions {
			task = task || (strings.Contains(e.TriggerSummary, "/sleep 150s") && e.State == "completed")
			notice = notice || (strings.HasPrefix(e.TriggerSummary, "Retirement notice") && e.State == "completed")
		}
		if !task || !notice {
			t.Fatalf("executions: task completed %t, notice completed %t: %+v", task, notice, d.Executions)
		}

		// The queued task went back to the lead, quoting it.
		if lead := systemMessages(t, get, orgID, "eng_lead"); !anyContains(lead, "Not delivered: seat contractor is retiring", "notes/second.md") {
			t.Fatalf("the lead was not told its task was not delivered: %q", lead)
		}
		// Both representatives got the summary.
		for _, rep := range []string{"representative_sean", "representative_alex"} {
			if !anyContains(systemMessages(t, get, orgID, rep), "Seat contractor has retired", "finished its retirement turn",
				"Hand over contractor", workID, "notes/second.md") {
				t.Fatalf("%s got no retirement summary", rep)
			}
		}
		// The work item is back to ready, unowned.
		out := f.delegateChain(t, `/tool work.get {"work_id":"`+workID+`"}`, "tool work.get", 4*time.Minute)
		if !strings.Contains(out, `"status":"ready"`) || !strings.Contains(out, `"owner":""`) || !strings.Contains(out, "owner contractor retired") {
			t.Fatalf("work item not released:\n%s", out)
		}
	})
}
