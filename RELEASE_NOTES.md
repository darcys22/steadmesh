# v0.8.0

## Changes since v0.7.0

This release makes agent configuration more portable while keeping each seat's model, tool and credential permissions explicit.

- **Portable reasoning effort** (#20): model profiles accept a common `effort` setting, translated into the pinned Claude Code, Codex or Pi runtime's native setting. Unsupported levels and conflicting native settings fail compilation. Omitting the field preserves existing defaults; equal effort labels do not imply equal cost or behaviour across models.
- **Policy-bound marketplace skills** (#16): declare public GitHub skill marketplaces at exact commits, select skills in Terraform, or allow a seat to discover and choose within a bounded catalog. Claude Code, Codex and Pi refresh the authorized managed set before turns and fail closed on loading or revocation errors. Skill selection does not grant tools, credentials or network access. Disposable workers do not inherit persistent skill-management authority.
- **Structured task results** (#19): disposable tasks can request a bounded, reference-free JSON schema and return validated JSON alongside their text result. Validation runs at the worker and again at the platform. Invalid output ends the task rather than replaying it; a schema-valid result remains untrusted content, not approval to act.
- **Explicit startup fallback** (#22, #27): a new seat can try approved alternate profiles when its primary executable is missing during preparation. Candidates share a pinned multi-harness image and a deadline, retain exact model/API grants, and persist the selected route before accepting work. Recovery stays on that route; revoked or changed bindings fail closed. The release workflow publishes the shared `seat-bundle` image for Linux amd64 and arm64. This does not switch after quota errors or retry an interrupted or side-effectful turn.
- **Attempt records and replay-safe failure handling** (#17): typed failures distinguish operational errors from valid negative answers. Each attempt records duration, execution/message identity, outcome and replay eligibility without exposing prompts or raw provider errors. The fallback helper shares the original deadline rather than resetting it per attempt.
- **Experimental ACP building blocks** (#18, #21, #23–26): bounded transport, isolated launching and a restricted lifecycle wrapper have real-runtime tests for exact model routing, platform tools, hostile workspace configuration, denied native permission requests, cancellation and portable recovery. ACP remains unregistered and unavailable as a production seat harness. Native managed skills and native session resume are not advertised; use the existing native adapters.

## Upgrade from v0.7.0

Back up Postgres, workspace volumes and Terraform state. Keep the chart, platform, controller, seat images, Terraform provider and orgctl on the same release. Apply the platform stage before the organization stage, then run `orgctl verify` with the explicit target context and organization. Wait for release publication to finish before using release-pinned artifacts.

Two new database migrations apply automatically when the platform opens the database:

- **00013** stores runtime skill selections separately from Terraform declarations.
- **00014** persists the selected harness profile and route binding with session checkpoints.

A Helm rollback does not undo these migrations. There is no supported automatic rollback; inspect migration history and logs before reverting binaries against a changed schema. If upgrading from before v0.7.0, follow the [v0.7.0 upgrade instructions](https://github.com/darcys22/steadmesh/blob/v0.7.0/RELEASE_NOTES.md) for controller quiescence, worker fencing and the credential ledger first.

Helm does not update existing CRDs from its `crds/` directory. Before applying organizations using the new skill, effort or startup-fallback fields, update both CRDs from the verified v0.8.0 checkout:

```sh
kubectl --context YOUR_CONTEXT apply -f charts/platform/crds/
```

Confirm both CRDs are established. This needs cluster-scoped CRD update permission; do not delete and recreate them.

Managed marketplace skills are opt-in. Their platform fetcher needs access to `codeload.github.com`; only public repositories pinned to full commits are supported. Hooks, MCP servers, dependencies and install scripts from marketplace plugins are not installed. Review skill contents before allowing them: instruction validation is not a sandbox for malicious advice.

Enable startup fallback only on a new seat. Resolve and pin the digest of the published `ghcr.io/darcys22/steadmesh/seat-bundle:0.8.0` image for every candidate profile. Legacy checkpoints lack the required profile binding and are not silently migrated. Follow the [startup-fallback configuration](https://github.com/darcys22/steadmesh/blob/main/docs/startup-fallback.md) for the approved chain and bundle-image requirements. No alternate credentials, tools or network permissions are implied by the chain.

## Verification and limits

CI covers unit/race tests, PostgreSQL recovery and migrations, Terraform examples and generated documentation. Startup-fallback conformance builds the bundle and runs its pinned native CLIs against scripted model endpoints with external networking disabled. Both bundle architectures are build-tested; native runtime conformance is exercised on amd64, not arm64. Task-worker test fixtures also run on Linux and macOS (#28). These checks are not a live production deployment or evidence that every provider/model supports every effort level.

See [marketplace skills](https://steadmesh.com/marketplace-skills.html), [structured task results](https://github.com/darcys22/steadmesh/blob/main/docs/structured-task-output.md), [startup fallback](https://github.com/darcys22/steadmesh/blob/main/docs/startup-fallback.md), and the [ACP contract](https://github.com/darcys22/steadmesh/blob/main/harnesses/acp/CONTRACT.md) for the supported boundaries.
