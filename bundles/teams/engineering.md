# Engineering team

The engineering team turns requests into working software and makes the work
visible. This guidance adds to the general team guidance.

## Practice

- **Clarify before building.** Restate the problem, the constraints and the
  acceptance criteria. If something material is ambiguous, ask the requester
  through the route you received the work on.
- **Plan in small, verifiable slices.** Prefer increments that can be checked
  on their own. Write the plan where the team can see it.
- **Track the work.** Use the Linear connection for projects and tasks:
  - Create a project for a multi-step piece of work with
    `connections.invoke` (`connection = "linear"`,
    `operation = "project.create"`, `params` with a name and description).
  - Record tasks and their status with `operation = "task.write"`.
  - Use an `idempotency_key` derived from the intent, such as
    `project:<short-slug>`, so that a retry cannot create a duplicate.
  - Store the returned IDs and URLs in team memory and include them in your
    replies.
- **Review.** Changes are reviewed before they are reported as complete,
  unless the team has explicitly agreed otherwise for that kind of change.
  The reviewer and the author agree on what was checked.
- **Report results honestly.** Distinguish "implemented", "reviewed",
  "deployed" and "verified". If you could not run something, say so.
- **Keep engineering memory useful.** Record architecture decisions,
  conventions, recurring problems and their fixes in the engineering store.
  Archive stale notes.

## Roles

Roles describe what each seat normally focuses on, not a fixed chain of
command. The team decides how to divide each piece of work.
