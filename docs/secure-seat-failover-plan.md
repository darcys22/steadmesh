# Secure seat failover integration plan

Status: design and remaining-work record. PR #17 implements the primitive and
attempt telemetry. The subsequent [startup fallback integration](startup-fallback.md)
implements explicit profiles, exact grants, a pinned bundle definition, durable
route checkpoints and missing-executable fallback before session startup.
Controller replacement and mid-turn quota failover below remain proposals.

## Current constraints verified in the code

- `pkg/spec/spec.go` defines one `HarnessProfile.Model`.
- `pkg/compile/compile.go` resolves one profile into each `SeatManifest`;
  model-use/readiness enumeration in `pkg/compile/required.go` is likewise
  primary-profile based.
- `services/api/seat.go` requires `model.infer` on the connection, then
  restricts the primary model connection to the selected model/API. Other
  explicitly granted connections currently allow arbitrary models. A fallback
  allowlist must not accidentally turn a newly granted connection into that
  unrestricted case.
- `cmd/seat-runner/runner.go` binds one adapter, model forwarder selection,
  event consumer and checkpoint owner during startup.
- `runtime/kube/render.go` renders one image and `STEADMESH_HARNESS`.
  The stock `build/seat-{claudecode,codex,pi}.Dockerfile` images are separate.
- Adapter sessions are persistent. A different process is not evidence of a
  fresh logical session, and a returned quota error is not proof that earlier
  model steps or tools did nothing.

## Proposed explicit configuration

Add an optional, platform-owned `fallback` block to a harness profile:

- `profiles`: ordered references to existing harness profiles, at most seven
  alternatives plus the primary. No recursive expansion; reject nesting,
  duplicate references and references to the primary.
- `on`: explicit operational triggers. Initial production support should be
  only `unavailable_before_start`; retain quota classification for telemetry
  until trusted pre-execution evidence exists.
- `max_attempts`: includes the primary, range 1..8, never greater than the
  approved chain length.
- `total_timeout`: required positive bounded duration for the entire startup
  chain, additionally limited by the parent execution/lease context.
- `image_digest`: an explicitly selected multi-harness image for in-pod
  failover, pinned under the existing production image rules.

Resolve references at compile time into immutable candidate records with
profile key, adapter, image digest, model connection/ID/API/settings, config,
required capabilities and a stable state namespace. Runtime agents cannot
supply, reorder or enlarge this list. Include the resolved list in the existing
manifest revision hash and generated schemas/deep copies/provider state.

Initial support should require all candidates to use the explicitly approved
multi-harness image. Do not substitute another candidate's image in a live pod,
download a missing CLI, or reinterpret an ordinary adapter image as a bundle.
Build the bundle from the existing pinned/digest-checked CLI installation
workflow, with the same non-root user, filesystem and network controls.
Preserve both candidate-specific requirements and the primary seat's required
capabilities. Reject a candidate that cannot meet the full capability envelope.

Keep the same workspace, seat identity, policy, sandbox and access-plugin
configuration. Candidate profiles must not be a vehicle for widening tool,
network, secret or filesystem access. The operator's explicit fallback
configuration approves the model destinations, not additional general powers.

## Exact model grants

The compiler should grant `model.infer` only on the approved candidate
connections. The proxy should treat every candidate connection as restricted,
checking an exact (connection, model ID, API) tuple against the committed
candidate allowlist. Never accept the Cartesian product of separately listed
models and APIs. An unrelated connection remains governed by existing explicit
permissions; fallback must not implicitly add it.

Tests must cover an allowed secondary tuple, unknown connection, unknown model,
wrong API, same-model/wrong-connection, empty model and stale manifest revision.
Task workers must not inherit the broader chain accidentally: either compile a
separate explicit task-worker candidate policy or keep their existing primary
selection and grants. Apply revocation using the current committed manifest,
not a stale pod copy. Accounting and per-execution output-token budgets remain
shared across attempts and connections.

Readiness must enumerate the approved candidate tuples, but the availability of
an alternative must not incorrectly prevent use of a healthy primary. Separate
configuration/authentication failures, which remain visible, from aggregate
availability of at least one usable candidate. Do not require all alternatives
to be healthy to declare the chain usable.

## Lifecycle state machine

Use one owner for the route and adapter lifecycle:

1. Load the committed manifest, bootstrap recovery and lease generation.
2. Resolve the persisted active candidate against the current approved list.
3. Before creating any adapter, establish the single shared deadline and
   freeze the allowed attempt order for this invocation.
4. Create and prepare the candidate, with its model endpoint routed through
   the existing credential-isolating local forwarder.
5. If preparation fails with trusted missing-executable evidence and before
   any native start or prompt submission, stop the partial adapter and drain
   its event consumer. Require successful cleanup before trying the next.
6. Start/resume only the selected candidate. Commit active-route identity
   before accepting deliveries, then make it the sole checkpoint/event owner.
7. Once a delivery starts, do not change route on crashes, missing responses,
   generic errors, cancellation or quota prose. Preserve existing platform
   acknowledgement/redelivery semantics.
8. Quiesce/stop/fencing cancel the shared context and prevent all subsequent
   candidates. No candidate receives a new deadline.

Failed cleanup is terminal, because leaving an old process alive makes a
second agent concurrent with it. Keep logging and event forwarding active
during cleanup, and never use successful cleanup as proof that earlier
external effects did not occur.

The active route must be durable and generation-fenced, not an in-memory
index. Record candidate/profile identity, model tuple, manifest revision,
adapter session-format identity and checkpoint identity together. On restart,
resume that exact approved route; do not silently switch back to the primary.
A native checkpoint for a revoked/unknown candidate must fail closed and
require explicit migration/recovery, not be loaded into a different harness.

When there is no prior checkpoint, startup failover can create a fresh session.
With an existing checkpoint, initial rollout should refuse a cross-candidate
switch. A later explicit portable-handoff mode must label the result as a new
session, preserve operation-ledger uncertainty, and never claim native resume.
Session state namespaces must distinguish two profiles using the same adapter
but different models/settings.

## Controller replacement alternative

If multi-harness images are not supported, route selection must be a
controller-owned desired-state transition rather than an in-pod adapter swap:

- Persist a compare-and-swap transition request with old generation, manifest
  revision, source candidate, target candidate and non-execution evidence.
- Quiesce and terminate the old pod before admitting a replacement; fence the
  old generation before issuing any new execution token.
- Render only the explicitly approved target image and exact target profile.
- Persist the chosen route before the new pod accepts work. A controller
  restart must not produce two active owners or reset the attempt budget.
- Mount persistent data under the existing security policy. Resume only the
  matching candidate's native checkpoint; otherwise require the explicit
  portable-handoff rules above.
- Fail closed on an uncertain pod-stop result, stale transition revision or
  fencing failure. Do not treat a network timeout as proof the old pod stopped.

This is a separate controller/storage migration with integration tests, not a
small runner patch. Prefer the bundle approach for the first bounded rollout.

## Required tests before enabling the feature

Existing PR #17 tests cover the primitive's bounds, deadline/cancellation,
typed-error policy, no-prose routing and replay vetoes. Integration coverage
still required:

- Compiler/provider/CRD round-trip, duplicate/cycle/nested profile rejection,
  revision hashing, pinned bundle image and capability-envelope enforcement.
- Exact secondary model grants, stale grants and revocation; no wildcard
  inference permission introduced for secondary connections.
- Fresh startup: missing primary binary to healthy secondary; one deadline;
  each candidate once; failed cleanup prevents transition.
- No cross-harness switch on auth/policy/configuration/session failure.
- Tool invocation followed by quota failure: no alternate delivery, no
  duplicate message, and unchanged acknowledgement semantics.
- Native checkpoint for the secondary survives restart and remains owned by
  that profile; unknown/revoked route fails closed.
- Concurrent quiesce, stop and fencing during prepare/cleanup never start the
  next adapter; old event streams close before new ownership is published.
- Budget accounting spans every attempt and cannot be reset by changing route.
- If controller replacement is selected: crash/restart and stale-generation
  tests around every persisted transition boundary, including uncertain pod
  termination and repeated reconcile calls.

## Rollout gate and remaining work

Keep fallback disabled by default. Enable launch-only failover only after the
configuration, exact grants, pinned bundle, durable route and lifecycle tests
above land together. Mid-turn quota failover remains out of scope until the
adapter/protocol can provide trustworthy proof that replay cannot duplicate a
tool or external effect. A quota classification alone is intentionally
insufficient.

This plan is not a claim of completed seat failover. The missing work spans
specification, compiler, provider/CRD generation, model proxy, seat lifecycle,
durable route storage, image construction and integration testing. It should
be implemented as coordinated reviewable changes rather than a permissive
runtime shortcut.
