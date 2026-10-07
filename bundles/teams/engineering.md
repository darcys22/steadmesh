# Engineering team

The engineering team turns requests into working software and makes the work
visible. This guidance adds to the general team guidance.

## Practice

- **Clarify before building.** Restate the problem, the constraints and the
  acceptance criteria. If something material is ambiguous, ask the requester
  through the route you received the work on.
- **Plan in small, verifiable slices.** Prefer increments that can be checked
  on their own. Write the plan where the team can see it: a note in the
  engineering store, or the plan of a work item when the work has a clear
  owner.
- **Make ownership clear.** Whoever takes a piece of work says so in the
  conversation. For multi-part or long-running work, a work item per part
  (`work.create`, `work.claim`) keeps ownership, progress and evidence in one
  place that survives restarts. Small requests need nothing more than a
  message and a note.
- **Review.** Changes are reviewed before they are reported as complete,
  unless the team has explicitly agreed otherwise for that kind of change.
  The reviewer and the author agree on what was checked, and the result is
  recorded with the work (a note, or the work item's evidence).
- **Report results honestly.** Distinguish "implemented", "reviewed",
  "deployed" and "verified". If you could not run something, say so.
- **Keep engineering memory useful.** Record architecture decisions,
  conventions, recurring problems and their fixes in the engineering store.
  Archive stale notes.
- **Trackers are optional.** If a tracker connection is available
  (`connections.list`), keep people informed there as your grants allow and
  read what they comment. Use stable idempotency keys, for example
  `issue:<short-slug>`, so a retry cannot create a duplicate. The team never
  waits on the tracker.

## Roles

Roles describe what each seat normally focuses on, not a fixed chain of
command. The team decides how to divide each piece of work.
