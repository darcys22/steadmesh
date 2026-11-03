# ACP compatibility spike (disabled)

## Status and scope

This package is **not a production harness**. It is deliberately absent from
`harnesses/all` and the registry. `Prepare`, `StartOrResume`, `Deliver`, `Quiesce`
and `Checkpoint` fail closed. No process is spawned, no Environment is retained,
no credential is written, and no default adapter changes. There is no enable flag.
`DescribeCapabilities` advertises no operational tools, streaming, recovery,
checkpoint or suspension capabilities. `Stop` only closes the event channel.

The initial spike includes an offline ACP v1 JSON-RPC wire decoder with
mocked exchanges. It binds output to a session and prompt, bounds record size,
normalizes text/tool observations, cancels every permission request, rejects
unsupported requests and updates, and requires a terminal prompt response.
It is intentionally strict and is not a complete ACP client. Tool observations
never execute tools. Permission replies are returned as bytes, never transmitted.
A decoder is single-use and single-threaded. EOF or any invalid record poisons it.
Callers must bound records *while reading*; the decoder cannot prevent an upstream
reader from allocating an oversized record. The follow-on streaming client is described below; the offline decoder remains
useful for isolated record tests.

## Functional transport and target configuration (follow-on)

`Client` now performs ACP v1 `initialize`, `session/new` and `session/prompt`
over an owned duplex transport. It handles real JSON-RPC request/response
correlation, bounded asynchronous framing, session-bound updates and deny-only
server requests. Context cancellation closes the transport, including blocked
writes. Known advisory updates are ignored without turning them into permissions
or accounting. Callers must supply deadline-bound contexts and a transport whose
Close unblocks reads and writes. A prompt callback must return promptly. Local turn tags cover already-read
records only: ACP v1 has no per-update turn ID, so the peer must obey the prompt
response boundary. These tags cannot identify stale bytes still in OS buffers.

`PlanCodex` constructs configuration for the official
`@agentclientprotocol/codex-acp` 2.5.0 with Codex 0.166.0. The plan pins the model
and Responses endpoint to a numeric loopback forwarder, uses a fixed placeholder
key rather than a provider credential (including the target's automatic API-key
authentication request), disables hooks/plugins in the session configuration,
sets read-only/on-request mode, supplies
the platform MCP bridge with the token-file path, retains authorized extra MCP
servers, and allow-lists access-plugin proxy environment settings. It copies no
ambient environment. Unsupported settings or environment overrides are errors.
The plan also supplies placeholder-only `StartupConfigTOML`. A trusted host must write it into a fresh private CODEX_HOME before launch: pinned ACP 2.5.0 otherwise applies the provider only to the thread, while native app-server startup defaults to openai. The integration fixture creates this file exclusively with mode0600. This plan is data, not permission to launch an executable.

`OpenCodexSession` negotiates the exact target identity/version and starts a fresh
session on a transport the trusted host already launched using that plan. It
checks managed-skill authorization before every prompt. Empty skill sets are
supported; nonempty sets and failed authorization refreshes fail closed. There
is no native-resume, checkpoint, or arbitrary-command API in this client.

These components are functional protocol building blocks, **not an enabled seat
adapter**. The original disabled lifecycle adapter remains unchanged. An agent's
self-reported version is not proof of executable integrity or confinement.

### Remaining production blockers

1. The target's session configuration automatically marks the workspace trusted.
   Its underlying Codex can load cwd-aware native config and hooks. Read-only
   model-tool mode alone does not prove startup hooks or native MCP config are
   confined. The launch boundary must neutralize or reject these sources with
   target-specific tests before any registration or process-launch integration.
2. Native managed-skill refresh and revocation still need a verified bridge.
   Do not silently replace that guarantee with prompt text or cached files.
3. Native session persistence, checkpoint ownership, process-group cancellation,
   quiesce ordering and portable recovery need end-to-end conformance.
4. Real target initialization passes with the pinned package identity and ACP
   v1, without model requests. `session/new` currently fails in this cloud test
   environment while loading AGENTS.md, with the native sandbox helper reporting
   `app-server socket directory must be a user-owned directory with mode 0700`.
   Retrying with private test directories mode 0700 produced the same failure.
   No model request ran. The root cause is not established; sandboxing was not
   disabled. A later clean isolated CI run passes session creation and a model-forwarder round trip after supplying native startup provider configuration. Tool execution, adversarial permission behavior and the other promotion gates remain unverified.

## Why not wrap acpx and enable it?

The inspiration is [no-mistakes internal/agent/acpx.go](https://github.com/kunchenguid/no-mistakes/blob/11947ea77b4b8ab8c9402eac689d07b1bcb6ee26/internal/agent/acpx.go),
which launches `acpx` and parses JSON event output. That implementation's
`--approve-all` is **not adopted**. Its generic model selection and session
behavior do not demonstrate Steadmesh's stronger seat contract. This is an
independent implementation against [ACP v1 prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn)
and [session setup](https://agentclientprotocol.com/protocol/v1/session-setup),
not a claim that raw ACP and a particular acpx CLI output are interchangeable.
No acpx CLI is used. Target package metadata is recorded in
`contract/codex-acp-runtime.json`; this is not a runtime-conformance claim.

## Compatibility gates before a runnable adapter

Each gate needs evidence for an exact target/version, not caller-supplied booleans
or an agent's capability assertion alone. Keep the current built-in adapters
until the full boundary is proven.

| Boundary | Required evidence | Current limitation |
| --- | --- | --- |
| Model proxy | Exact model ID/API/settings and every request routed through `Environment.Model.BaseURL`; placeholder key only; test no native provider fallback | ACP model selection does not standardize Steadmesh endpoint configuration. `PlanCodex` maps the pinned target to a loopback Responses forwarder; real-agent verification is still required. |
| Credentials and egress | Clean HOME/environment, platform token-file rotation, existing access-plugin proxy controls, no provider keys in the sandbox | Generic acpx invocation/inherited environment cannot establish isolation. This spike never launches it. |
| Permissions | A seat-authority-aware request bridge with explicit denied/unknown handling; no blanket approvals; audit correlation | Only offline cancelled replies exist. Filesystem, terminal and other agent requests are rejected. |
| Tools | Platform tool bridge plus authorized extra MCP servers, unchanged token/fencing authority and transport semantics; real readiness probe | ACP session/new supports MCP configuration, but a real transport/session client and stdio server mapping now exist; production launch and authority conformance remain unverified. Tool events are informational only. |
| Skills/instructions | Trusted bootstrap and per-turn `LoadSkills` refresh/revocation, fail closed on loader errors, correct native instruction precedence | No target-neutral mechanism proves skill revocation or prevents stale native caches. |
| Recovery | Durable seat-volume session state, format/version identity, config revision checks, unknown-operation reconciliation, explicit portable handoff | ACP `loadSession` alone is not a Steadmesh application checkpoint. No recovery guarantee is advertised. |
| Lifecycle | Serialized Deliver, concurrent Stop/Quiesce, bounded I/O, cancellation/timeout escalation, process-group cleanup and drained event ordering | The offline decoder does not own a process or claim operational interruption. |
| Protocol/runtime | Digest/version-pinned CLI and target, initialized protocol/capability negotiation, bounded framing, target-specific recorded conformance fixtures | Tests include full duplex ACP v1 exchanges with a scripted peer. Numeric prompt IDs, non-text content and unknown update variants are deliberately unsupported. |

## Wire outcomes and accounting

Only a correlated `session/prompt` result ends a mock turn. The decoder preserves
`end_turn`, `max_tokens`, `max_turn_requests`, `refusal` and `cancelled` separately;
it does not map all termination to Steadmesh `TurnCompleted`. A future adapter
must specify the mapping and test interruption precedence. Output and tool events
are observations, not business completion. Context-window usage is not assumed
to be per-turn billable usage; no accounting values are invented. Raw remote
errors and arbitrary tool payloads are not copied into error strings or events.

## Validation and promotion

Run `go test -race ./harnesses/acp`, `go vet ./harnesses/acp`, `go build ./...`,
and `go test -short ./...` with the repository's required Go toolchain.
Mock tests cover cancellation-only permission replies, exact large request IDs,
wrong sessions/IDs, malformed and oversized records, unsupported agent requests,
non-success stop reasons, incomplete EOF and a permanently disabled lifecycle.
Additional duplex tests cover handshake/version validation, session creation,
permission cancellation with exact large IDs, unsupported requests, bounded
backpressure under output bursts, terminal-response draining before clean EOF,
rejection of already-buffered stale output, cancellation
during a blocked write, malformed framing, wrong-session updates, incomplete EOF
and per-turn skill-authorization failure. Configuration tests verify proxy-only
model routing, placeholder credentials and environment/MCP override rejection.
The duplex-peer tests are not real-agent conformance or a security audit.

An opt-in integration test runs installed, explicitly selected pinned binaries
against a local scripted model through the real forwarder. It inherits no user
credentials, uses a dummy token and placeholder API key, and rejects proxy-aware
external HTTP(S) traffic. Set `STEADMESH_ACP_NODE`, `STEADMESH_ACP_SCRIPT` and
`STEADMESH_ACP_CODEX_BIN` to absolute paths, then run:

```
go test -tags integration -run TestCodexACPProtocolConformance/Initialize -v ./harnesses/acp
go test -tags integration -run TestCodexACPProtocolConformance -v ./harnesses/acp
```

The first command passed in the cloud workspace. The full command was initially
blocked there at `session/new`; the isolated CI image now passes the complete
model-forwarder round trip without weakening sandboxing.
Without explicitly selected runtimes this test skips; a skipped test is not
runtime verification.

Promotion requires a separate reviewed change: pin one real target, implement
and verify every boundary above with the real model forwarder and tool bridge,
then register it and add runtime image/profile support. Do not enable first and
silently downgrade missing capabilities later.

### Isolated CI runtime check

`ACP isolated conformance` builds a test-only image with the approved package
versions and an integrity-pinned npm dependency tree. It runs the complete real
runtime fixture with no external network, no host mounts or credentials, a
read-only root filesystem and a private writable `/tmp`. It does not relax the
container or native sandbox. The test must reach the local model forwarder and
assert its model and seat authority; initialization alone does not pass the job.
A sandbox/preflight failure is a failing check, not an optional skipped success.
This job still does not enable the production adapter or prove all promotion
gates. The clean-runner outcome is recorded below.

The first isolated CI run built the image, initialized ACP, and created a session
without the cloud sandbox preflight failure. It then exposed a pinned-target
provider mismatch before any model response: the thread used `steadmesh`, but
the native app-server process still used its default `openai` provider. The
follow-up startup configuration above addresses this mismatch. This is distinct
from the earlier cloud preflight failure.

The corrected run [41742408565](https://github.com/darcys22/steadmesh/actions/runs/41742408565)
passed the complete fixture at commit479fa56: exact runtime initialization,
session creation, streamed local-model output, and model ID plus seat token,
generation and execution assertions. It made one request to the local modelstub,
with external networking disabled. This verifies that narrow round trip, not
real-model quality, native tool permissions, workspace-config safety, managed
skills or durable recovery. The production adapter remains unregistered.
