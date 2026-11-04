# Explicit startup fallback

Startup fallback lets a **new seat** try an explicitly approved alternate
harness when the primary executable is missing. It is opt-in and applies only
to `Prepare`, before a native session starts or a message is delivered.

It does not retry a failed turn, switch on quota/provider/authentication errors,
or migrate an existing session between harnesses. The ordinary single-harness
path is unchanged when no policy is configured.

## Configure an approved chain

Releases publish `ghcr.io/darcys22/steadmesh/seat-bundle:<version>` for
`linux/amd64` and `linux/arm64`. Resolve and pin the published image digest,
just as for the single-harness seat images. For a custom image, build
`build/seat-bundle.Dockerfile` using `build/build.sh seat-bundle` and publish it
through your own image pipeline. The bundle includes the same pinned Claude Code, Codex and Pi runtimes as the
separate seat images, with the same non-root user and platform token routing.
No runtime download or installation is performed.

Both profiles must select that same bundle image. For example, inside an
organization's Terraform `harness_profiles`:

```hcl
primary = {
  adapter      = "codex"
  image_digest = var.pinned_seat_bundle_image
  model = {
    connection = "openai"
    id         = "your-approved-model"
    api        = "openai_responses"
  }
  startup_fallback = {
    profiles      = ["secondary"]
    max_attempts  = 2
    total_timeout = "90s"
  }
}
secondary = {
  adapter      = "pi"
  image_digest = var.pinned_seat_bundle_image
  model = {
    connection = "openai"
    id         = "your-approved-model"
    api        = "openai_responses"
  }
}
```

The model IDs and APIs must already be declared on the referenced connection.
Assign the seat to `primary`. A healthy primary is always used first on fresh
startup; the alternate is not a load-balancing route.

The policy allows one to seven ordered alternatives and one to eight attempts,
including the primary. The attempt limit cannot exceed the chain length. The
shared timeout must be positive and at most five minutes. Self-references,
duplicates, nested fallback, different images and weaker required capabilities
are rejected. If the seat uses managed skills, every candidate must support
managed skill loading and revocation.

## What is authorized

The compiler resolves the entire list into the immutable seat manifest. It
grants inference only on candidate connections. For those connections the
proxy accepts only exact approved `(connection, model ID, API)` tuples, not
arbitrary models or a cross-product of model/API lists. Credentials remain
outside the seat and all inference still uses the existing model proxy and
budget accounting. Task workers keep their existing primary-only selection;
the parent seat's fallback list is stripped from worker manifests.

The policy authorizes the profiles' model destinations, not additional tools,
network, credentials or filesystem access. The seat retains its sandbox,
identity, access plugins and permission policy. The primary model connection's
existing readiness requirement remains in force: this feature addresses a
missing CLI, not provider unavailability. Secondary connections are not made
required by default merely by appearing in the fallback list.

## Lifecycle and persistence

Every attempt uses one shared cancellation/deadline context. A missing
executable from `Prepare` can advance only after the failed adapter stops and
its event stream drains. Failure or uncertainty during cleanup is terminal.
No error from `StartOrResume` or `Deliver` advances the chain. Cancellation,
fencing, unknown prior operations, a stale manifest, policy/authentication
errors and ambiguous error evidence also stop the chain.

A successfully started candidate is checkpointed to the platform **before**
accepting a message. The checkpoint stores profile identity and a binding hash
covering adapter, model, settings, image, configuration and capabilities.
Checkpoint writes retain lease-generation fencing. Bootstrap returns the bound
route even if it was subsequently revoked, allowing the runner to refuse it
rather than silently restart on the primary.

Restart resumes only the recorded approved candidate. Reordering the chain
will not switch back to the primary. Removing the route or changing its bound
configuration fails closed. Profile-specific home and runner directories keep
native state separate even when two profiles use the same adapter. The shared
seat workspace remains unchanged.

An existing legacy checkpoint has no trustworthy model/profile binding and is
not automatically migrated. Enable this feature on a new seat. Session
migration, a controller-driven image change, and mid-turn failover need separate
explicit workflows; this feature does not pretend to implement them.

## Verification

Tests cover exact secondary model grants, configuration/schema round-trips,
capability preservation, primary-only task workers, stop/drain ordering, shared
deadlines, cancellation/fencing, sticky recovery, revocation, legacy-session
rejection and no replay after a failed turn. PostgreSQL/API integration tests
cover durable route round-trip and checkpoint-to-bootstrap recovery.

The bundle's pins and security settings are checked by tests. The startup
fallback CI workflow builds the container and exercises each pinned CLI against
scripted model endpoints with external networking disabled. Operators should
retain these checks in their image pipeline before deploying a custom bundle;
a successful Go unit test alone is not a container or live-conformance pass.
