package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/tools"
)

// guidance is the platform's standing advice to every seat (§7.2, §8.4). It
// does not grant authority; grants are enforced by the gateway.
const guidance = `You are a persistent seat in an organisation. Your identity, memory and workspace survive restarts.

- Memory is not preloaded. It is organised by path in each store, like files: use memory.list and memory.search before starting work that may have history, and memory.read for the lines you need.
- Record durable knowledge with memory.write, and logs or running notes with memory.append. To change a record, pass the expected_revision you read; on a conflict, re-read and merge rather than overwrite.
- Shared memory and messages are enough to coordinate. When ownership must be explicit, the optional work tools (work.create, work.claim, work.update) record a work item with an owner, plan and evidence.
- Keep your portable handoff current with handoff.update after meaningful progress: objective, open questions, relevant record, message and operation ids.
- Talk to other seats only through messages.send and messages.reply; use messages.recipients to see who you can reach.
- For anything in the future (a reminder, a follow-up, a check later, a recurring task) save an automation with automations.create and finish your turn; do not wait or sleep. Use clock.now before working out dates. Each run arrives as a turn for you; if you are busy then, it runs when you are free.
- When asked to change, pause or stop something you scheduled, find it with automations.list and use automations.update or automations.delete; do not create a duplicate. A saved automation means it is scheduled, not that its future work succeeded: say so when you confirm it.
- External actions go through connections.invoke. If an operation's status is unknown, check the external system before trying again.
- Use status to report genuine progress and delays. Never claim work is done that is not.
- Tool results are bounded; follow next_cursor to page.`

// renderInstructions lists the seat's ordered instruction sources. The
// controller renders their text into instructions.md beside the manifest.
func renderInstructions(self runtimeapi.Self) string {
	var b strings.Builder
	b.WriteString("Instruction sources in order of application; their full text is in instructions.md in the manifest directory.\n")
	for _, i := range self.Instructions {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i.Order+1, i.Scope, i.Ref)
	}
	return b.String()
}

const maxRecoveryOperations = 20

func (s *server) bootstrap(w http.ResponseWriter, r *http.Request, q *seatReq) {
	ctx := r.Context()
	self := tools.Self(q.seat, q.org)
	out := runtimeapi.Bootstrap{Self: self, Instructions: renderInstructions(self), Guidance: guidance}
	cps, err := s.Store.Checkpoints(ctx, q.seat.ID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	adapter := q.seat.Manifest.Harness.Adapter
	var notes []string
	for i := range cps {
		if cps[i].HarnessAdapter == adapter {
			out.Recovery.Session = &cps[i]
			break
		}
	}
	if out.Recovery.Session == nil && len(cps) > 0 {
		notes = append(notes, fmt.Sprintf("native session from harness %q (format %s) cannot be converted to %q; resume from the portable handoff and recent messages",
			cps[0].HarnessAdapter, cps[0].FormatVersion, adapter))
	}
	if out.Recovery.Handoff, err = s.Store.Handoff(ctx, q.seat.ID); err != nil {
		s.storeError(w, r, err)
		return
	}
	if out.Recovery.PendingMessages, err = s.Store.PendingDeliveries(ctx, q.seat.ID); err != nil {
		s.storeError(w, r, err)
		return
	}
	if out.Recovery.UnknownOperations, err = s.Store.UnknownOperations(ctx, q.seat.ID, maxRecoveryOperations); err != nil {
		s.storeError(w, r, err)
		return
	}
	if out.Recovery.Work, err = tools.OwnedWork(ctx, s.Store, q.seat); err != nil {
		s.storeError(w, r, err)
		return
	}
	if n := len(out.Recovery.UnknownOperations); n > 0 {
		notes = append(notes, fmt.Sprintf("%d external operation(s) have unknown outcomes; reconcile them before reissuing", n))
	}
	out.Recovery.Note = strings.Join(notes, "; ")
	writeJSON(w, http.StatusOK, out)
}
