# v0.7.0

## Changes since v0.6.0

- **Composable native teams** (#15): declared leaders and independently running native pool members can coordinate on an explicitly shared work board. Pool bounds, mission editing, peer routes, distinct-participant review and context-edit permissions remain separate operator choices. Versioned private context supports direct self-edits or instruction proposals approved by a different authorized teammate. Hybrid, self-organising and fixed-prompt examples are included.
- **Bounded elastic teams** (#13): opt-in temporary worker fan-out with shared organization and execution limits, explicit worker roles, positive output-token budgets, durable capacity accounting and one-shot launch fencing. Terraform schema, examples and generated provider documentation include the new configuration.
- **Representative MCP gateway** (#12): optional external-agent conversations through a dedicated, authenticated gateway with caller-scoped conversation and idempotency records. Disabled by default.
- **Permission hardening** (#11): independent grant clauses, strict adapter target validation, scoped credential delivery and an encrypted, durable issued-credential ledger with revocation retries.

## Upgrade notes

Keep the chart, platform, controller, seat images, provider and orgctl on v0.7.0. Update module references and provider constraints together, apply the platform stage before the organization stage, and run `orgctl verify` with the explicit target context and organization. Wait for the release workflow to finish publishing the chart, images and signed provider before using the release-pinned quickstart.

Back up Postgres, workspace volumes and Terraform state before upgrading. Pending migrations 00005 through 00012 apply automatically when the platform opens the database:

- 00005: durable issued-credential ledger
- 00006: external representative conversations
- 00007: elastic-team capacity accounting
- 00008: one-shot worker launch fence
- 00009: independent native-pool membership
- 00010: versioned participant context and history
- 00011: team missions and message execution attribution
- 00012: reviewed instruction proposals and approver attribution

There is no built-in backup/restore or supported automatic rollback. A Helm rollback does not reverse these database migrations. Inspect migration history and platform logs if startup fails; do not remove migrations or roll binaries back against an unverified schema.

Before the migration/restart, stop new worker creation, drain or cancel existing workers, confirm their Pods are gone and quiesce every old controller. Do not mix v0.6.0 controllers with v0.7.0: old controllers do not participate in launch fencing. Bring up the matching release only after old controllers and their outstanding Kubernetes API requests cannot issue further creates; recheck worker Pods after quiescence.

Helm does not upgrade existing CRDs from its crds/ directory. Before applying organizations using elastic_teams or native coordination, pool and context policies, update both existing CRDs from a verified v0.7.0 checkout with `kubectl --context YOUR_CONTEXT apply -f charts/platform/crds/` and confirm they are established. This requires cluster-scoped CRD update permission. Do not delete/recreate the CRDs.

Provision and preserve a stable CREDENTIAL_LEDGER_KEY through the existing Secret reference before sandbox credential delivery. Use a coordinated controller/platform restart, resync every organization to recompile grants and review grants that stricter validation rejects. Previously issued tokens cannot be retroactively recorded; revoke old GitHub App tokens upstream or allow them to expire. PATs and browser sessions require separate upstream handling.

A worker launch that crashes after acquiring its fence but before resolving it, or has an ambiguous create outcome, retains its capacity reservation. A missing Pod is not enough to prove that a delayed create cannot arrive. Manual operator recovery must first establish that the original launch can no longer create resources; there is no automatic fence reset or supported recovery command. Do not retry the same launch or clear its reservation solely because its Pod is absent.

Legacy disposable workers inherit the requester's model/runtime authorization and support only the documented read-only tools. Native pool members instead have independent seat identities, inboxes and ordinary turns under their explicit seat templates, grants and routes. Existing elastic-team policies and task records are not automatically converted to native pools. Omitting native policies preserves declared-team behavior; context editing is off by default.

Native pools require positive organization-daily and execution output-token budgets. Pool bounds and per-execution message limits constrain different resources; output-token budgets do not cap input tokens or monetary spend. Native `retention_seconds = 0` retains retired private data indefinitely. A positive value explicitly enables deletion of retired members' private context, memory, sessions and handoffs after that interval; shared evidence and message audit remain. Choose retention deliberately before applying a pool policy. The representative MCP gateway remains opt-in.

Focused native-team CI covers builds, unit and race tests, PostgreSQL migration/concurrency regressions, Terraform example validation and generated documentation. The example smoke input uses the deterministic fake harness; it is not evidence of model-driven autonomy or a new full live-services deployment.

See [operations and upgrade instructions](https://steadmesh.com/operations.html#v070-upgrade), [native-team policies and lifecycle](https://steadmesh.com/native-teams.html), [elastic-team configuration](https://steadmesh.com/configuration.html#elastic-teams) and [task-worker limits](https://steadmesh.com/task-workers.html).
