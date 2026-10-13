# Compare independent run alternatives

Run branching builds on [disposable task workers](task-workers.md). It starts
2–4 independent runs with the same explicit handoff and optional text files,
then lets the parent compare their results and deliberately select one.

This is a portable handoff snapshot, not a clone of process RAM, a harness
session, or a Git working tree. It does not discover or copy local files,
credentials, browser sessions, environment variables, or parent tool grants.
Supply only the non-secret context and files that each alternative needs.
Every child uses the disposable-worker model, runtime and sandbox boundaries.

## Start alternatives

Call `branches.start` through `steadmesh-tools`, MCP (`branches_start`), or the
normal `/v1/tools/branches.start` endpoint. The caller must have a current seat
lease and a running parent execution; the normal runner supplies both headers.

```sh
steadmesh-tools call branches.start '{
  "request_id": "b8b66951-bab1-4519-b58b-4c54b6096370",
  "handoff": "Implement a formatter. Preserve all lines and avoid new dependencies. Write your proposed output to result.txt, and explain your tradeoffs.",
  "input_files": [{"path": "fixture.txt", "content": "beta\nalpha\n"}],
  "artifact_paths": ["result.txt"],
  "timeout_seconds": 600,
  "alternatives": [
    {"name": "direct", "prompt": "Try direct string processing"},
    {"name": "table-driven", "prompt": "Try a table-driven approach"}
  ]
}'
```

Use a fresh UUID for a new request and reuse that UUID with the identical
payload when retrying a lost response. A changed payload conflicts instead of
launching more work. The whole group is transactional: if any alternative is
invalid or a worker limit is reached, no group or partial set of workers is
created. Workers with runtime cleanup outstanding count against the same four-worker
limit, including completed or cancelled workers awaiting cleanup. Branching
does not bypass per-parent execution limits.

The returned `id` identifies the durable group. `snapshot_sha256` identifies
the shared handoff and exact input files. The alternatives each have their own
`task_run_id`, prompt, empty workspace and lifecycle. Their original request
and results remain available after their disposable Pods are removed.

## Compare and review

```sh
steadmesh-tools call branches.compare '{"id":"GROUP_ID"}'
```

Comparison reports each alternative in the original order with:

- its name, task-run ID and current state
- UTF-8-safe output and error previews, with explicit truncation flags
- artifact count and total content bytes
- the SHA-256 of the complete result, including artifact names and contents
- the current group `revision`, any selection, and `all_terminal`

Previews are deliberately bounded to fit the platform tool-result limit. Use
`tasks.get` to inspect a task, including its artifact list, and the authenticated
`GET /v1/task-runs/TASK_RUN_ID` API to retrieve its complete immutable request
and result. Review full output and artifacts before making a choice. Technical
completion alone does not mean the generated work is correct.

## Explicit selection

```sh
steadmesh-tools call branches.select '{
  "id": "GROUP_ID",
  "task_run_id": "CHOSEN_TASK_RUN_ID",
  "expected_revision": 1,
  "expected_result_sha256": "RESULT_SHA256_FROM_COMPARISON"
}'
```

Only a completed alternative belonging to this group and this parent seat can
be selected. Failed, cancelled, timed-out and unfinished runs cannot be chosen.
A stale group revision or incorrect result digest conflicts, so reread and
review before retrying. Choosing the same current result is a no-op; choosing
a different completed result creates a new revision and preserves choice
history in the store.

Selection is **a durable choice record only**. It does not apply artifacts,
modify the parent's working tree or handoff, merge a Git branch, send messages,
grant permissions, or cancel other alternatives. `all_terminal` and `selection`
are independent: a result may be chosen while another alternative still runs.
Cancel unwanted siblings explicitly with `tasks.cancel`. Apply reviewed files
through a separate, explicitly authorized workflow, including any normal
working-tree conflict checks.

## Limits and recovery

- Two to four alternatives; names are unique ignoring case and up to 64 bytes
- Shared handoff and each alternative prompt are at most 8 KiB of UTF-8 text
- Shared text files and artifact paths use the task-worker validation and bounds
- Default deadline 600 seconds; configurable from 1 to 1,200 seconds
- No platform tools are delegated by branching in this first version
- The authenticated parent seat owns the group; another seat cannot inspect or select it
- Creation and selection are fenced by the parent's current lease generation
- Read-only comparison is a consistent database snapshot; it cannot observe half a group
- Ending the parent turn does not cancel children while its lease generation remains valid; the parent stays warm until child cleanup completes
- Parent restarts retain group records and selections; worker cancellation and cleanup follow the disposable-worker lifecycle

The feature requires the usage-budget migration `00002`, then the
disposable-worker migration `00003`, then `00004_run_branches.sql`, in that
order. Merge and deploy the budget and worker prerequisites first; adding a
missing lower-numbered migration after later ones have already been applied
is rejected by the migration runner. No Kubernetes, credential or Terraform
changes are needed specifically for branching beyond those prerequisites.
