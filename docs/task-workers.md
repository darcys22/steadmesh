# Disposable task workers

Migration order: deploy the model-budget migration `00002` before this
feature's `00003`. Run branching builds on `00004` afterward. The worker PR
is stacked on the budget PR to preserve this upgrade order. Do not deploy
the worker branch independently before its base.

A persistent seat can delegate a bounded piece of work to a fresh worker,
then inspect its output and explicitly chosen artifacts. The parent keeps
its identity, workspace, memory, harness session and one-turn lease. A
worker is a separate short-lived Kubernetes Pod, not another persistent seat.

## Start, inspect and cancel

During an active parent turn, call `tasks.start` with a unique UUID
`request_id`, a `prompt`, optional `input_files` (`path`, `content`), and
`artifact_paths` to return. Repeating an identical request ID returns the
same run; reusing it for different input fails. Use `tasks.get` with its `id`
to inspect state, bounded output and artifact names, and `tasks.cancel` to
stop it. When runtime cleanup completes, a durable, deduplicated system
message wakes the parent with the task ID and parent execution correlation.
That message reports technical completion, not business success.

Example arguments (illustrative):

```json
{
  "request_id": "814cdd22-73ed-4d55-8bb7-02b685291dbb",
  "prompt": "Review input.txt and write a short critique to review.txt",
  "timeout_seconds": 300,
  "input_files": [{"path": "input.txt", "content": "An explicit draft to review"}],
  "artifact_paths": ["review.txt"]
}
```

The runtime API also offers `POST /v1/task-runs`, `GET /v1/task-runs`,
`GET /v1/task-runs/{id}`, and `POST /v1/task-runs/{id}/cancel`.
Creation requires the current generation and a real running parent execution
in the existing runtime headers. Reads are scoped to the parent seat. The
full GET returns the explicit input and result files; tool results retain
the existing 16 KiB limit. `tasks.get` with `artifact_path` returns a single
small artifact; use the runtime API for larger files.

## Bounds and authority

- At most four workers with runtime cleanup outstanding per seat, and
  sixteen creations per parent execution, enforced in a serialized transaction
- Default deadline 600 seconds, maximum 1,200, including provisioning time;
  the Pod also has an active deadline and never restarts
- Prompt at most 32 KiB; at most 64 UTF-8 files totaling 256 KiB in each
  direction; output at most 16 KiB; files must use safe relative paths
- Only requested regular-file artifacts are returned; missing, binary,
  oversized or escaping paths fail the run. `.git` input paths are rejected
- Fresh, size-limited `emptyDir` workspace, home and scratch, with no parent
  PVC, manifest mount, session, shared workspace or sandbox credentials
- Same pinned harness image, model selection, resource limits, security
  context and RuntimeClass as the parent; no runtime downgrade
- Dedicated ServiceAccount and gateway-audience, Pod-bound projected token;
  no Kubernetes API token or additional RBAC. Worker tokens cannot acquire
  the parent lease, read its bootstrap, checkpoint, send messages, obtain
  credentials, spawn workers or call arbitrary tools
- Default no platform tools. `tool_names` may select only `memory.stores`,
  `memory.list`, `memory.read`, `memory.search`, `memory.history`, `work.get`
  and `work.list`; the current parent's read/search permissions still apply
- Separate default-deny network-policy selector allows only platform and
  DNS traffic, never the parent's extra network or egress-gateway access

Outstanding workers keep an otherwise idle parent runtime warm until cleanup
finishes, so a completion notification cannot cause an idle restart that
revokes still-running siblings. This does not bypass suspension or blocking.

Changing the parent's configuration revision or lease generation, retiring
it, or reaching the task deadline revokes worker authentication. Suspending
or blocking the parent causes the controller to cancel its workers. A claimed
worker whose Pod disappears fails rather than silently repeating external
work. Cancellation is immediate at the API boundary; Pod removal remains
asynchronous. Capacity is reusable only after the controller confirms Pod
absence and deletes its identity and policy.

Results remain durable after cleanup. Nothing copies results into the
parent workspace or changes shared memory automatically. The parent decides
what to use. Deleting an organisation with data deletion also removes its
worker records. No production cluster or credentials are needed to run the
unit and PostgreSQL integration tests.

This v1 intentionally uses explicit text snapshots and artifacts. It does
not clone repositories, copy live files automatically, resume worker
sessions, delegate credential access or support nested workers.
