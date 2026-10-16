# v0.7.0

## Changes since v0.6.0

- **Bounded elastic teams** (#13): opt-in temporary worker fan-out with shared organization and execution limits, explicit worker roles, positive output-token budgets, durable capacity accounting and one-shot launch fencing. Terraform schema, examples and generated provider documentation include the new configuration.
- **Representative MCP gateway** (#12): optional external-agent conversations through a dedicated, authenticated gateway with caller-scoped conversation and idempotency records. Disabled by default.
- **Permission hardening** (#11): independent grant clauses, strict adapter target validation, scoped credential delivery and an encrypted, durable issued-credential ledger with revocation retries.

## Upgrade notes

Keep the chart, platform, controller, seat images, provider and orgctl on v0.7.0. Back up Postgres, workspace volumes and Terraform state before upgrading. Pending migrations 00005 through 00008 apply when the platform opens the database; automatic rollback is not provided.

Before the migration/restart, stop new worker creation, drain or cancel existing workers, confirm their Pods are gone and quiesce every old controller. Do not mix v0.6.0 controllers with v0.7.0: old controllers do not participate in launch fencing. Bring up the matching release only after old controllers and their outstanding Kubernetes API requests cannot issue further creates; recheck worker Pods after quiescence.

Helm does not upgrade existing CRDs from its crds/ directory. Before applying organizations using elastic_teams, update the existing CRDs from a verified v0.7.0 checkout with `kubectl --context YOUR_CONTEXT apply -f charts/platform/crds/` and confirm they are established. This requires cluster-scoped CRD update permission. Do not delete/recreate the CRDs.

Provision and preserve a stable CREDENTIAL_LEDGER_KEY through the existing Secret reference before sandbox credential delivery. Use a coordinated controller/platform restart, resync every organization to recompile grants and review grants that stricter validation rejects. Previously issued tokens cannot be retroactively recorded; revoke old GitHub App tokens upstream or allow them to expire. PATs and browser sessions require separate upstream handling.

A worker launch that crashes after acquiring its fence but before resolving it, or has an ambiguous create outcome, retains its capacity reservation. A missing Pod is not enough to prove that a delayed create cannot arrive. Manual operator recovery must first establish that the original launch can no longer create resources; there is no automatic fence reset or supported recovery command. Do not retry the same launch or clear its reservation solely because its Pod is absent.

Workers inherit the requester's model/runtime authorization and support only the documented read-only tools. Output-token budgets do not cap input tokens or monetary spend. The representative MCP gateway remains opt-in.

See [operations and upgrade instructions](https://steadmesh.com/operations.html#v070-upgrade), [elastic-team configuration](https://steadmesh.com/configuration.html#elastic-teams) and [task-worker limits](https://steadmesh.com/task-workers.html).
