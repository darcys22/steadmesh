# ACP compatibility spike (disabled)

## Status and scope

This package is **not a production harness**. It is deliberately absent from
`harnesses/all` and the registry. The original disabled `Adapter` methods
`Prepare`, `StartOrResume`, `Deliver`, `Quiesce` and `Checkpoint` fail closed. No process is spawned, no Environment is retained,
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
The plan also supplies placeholder-only `StartupConfigTOML`. A trusted host must write it into a fresh private CODEX_HOME before launch: pinned ACP 2.5.0 otherwise applies the provider only to the thread, while native app-server startup defaults to openai. The integration fixture creates this file exclusively with mode0600. The same file configures required platform-authorized MCP bridges with bridge-specific approval, while native ACP permissions remain deny-only. Session MCP declarations are empty to avoid the pinned target treating duplicate native/session definitions as failed startups. MCP names must be canonical ASCII letters, digits, underscores or hyphens. This plan is data, not permission to launch an executable.

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
4. Real target initialization, session creation, model routing and the platform
   self tool pass in a clean isolated CI container. The earlier cloud sandbox
   helper error was environment-specific; no security setting was weakened.
   Production promotion still requires adversarial workspace-configuration,
   permissions, cancellation and recovery evidence. Workspace config can override
   user startup config and recursively preserve MCP environment fields, so a
   provider round trip or MCP name allowlist alone is not enough.

## Why not wrap acpx and enable it?

The inspiration is [no-mistakes internal/agent/acpx.go](https://github.com/kunchenguid/no-mistakes/blob/11947ea77b4b8ab8c9402eac689d07b1bcb6ee26/internal/agent/acpx.go),
which launches `acpx` and parses JSON event output. That implementation's
`--approve-all` is **not adopted**. Its generic model selection and session
behavior do not demonstrate Steadmesh's stronger seat contract. This is an
independent implementation against [ACP v1 prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn)
and [session setup](https://agentclientprotocol.com/protocol/v1/session-setup),
not a claim that raw ACP and a particular acpx CLI output are interchangeable.
No acpx CLI is used. Target package metadata is recorded in
`contract/codex-acp-runtime.json`; the recorded runtime evidence is deliberately limited to its stated checks.

## Compatibility gates before a runnable adapter

Each gate needs evidence for an exact target/version, not caller-supplied booleans
or an agent's capability assertion alone. Keep the current built-in adapters
until the full boundary is proven.

| Boundary | Required evidence | Current limitation |
| --- | --- | --- |
| Model proxy | Exact model ID/API/settings and every request routed through `Environment.Model.BaseURL`; placeholder key only; test no native provider fallback | ACP model selection does not standardize Steadmesh endpoint configuration. `PlanCodex` maps the pinned target to a loopback Responses forwarder; isolated real-agent routing is verified; malicious inherited config remains a promotion gate. |
| Credentials and egress | Clean HOME/environment, platform token-file rotation, existing access-plugin proxy controls, no provider keys in the sandbox | Generic acpx invocation/inherited environment cannot establish isolation. This spike never launches it. |
| Permissions | A seat-authority-aware request bridge with explicit denied/unknown handling; no blanket approvals; audit correlation | Only offline cancelled replies exist. Filesystem, terminal and other agent requests are rejected. |
| Tools | Platform tool bridge plus authorized extra MCP servers, unchanged token/fencing authority and transport semantics; real readiness probe | ACP session/new supports MCP configuration, but a real transport/session client and stdio server mapping now exist; isolated real self-tool execution is verified; production launch and adversarial authority conformance remain unverified. Tool events are informational only. |
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

### Experimental one-turn launcher (Linux only)

`ExperimentalLauncher` is a host-facing helper, not a registered adapter. It
checks root-owned, non-group/world-writable canonical runtime paths and an empty
neutral cwd, including its ancestors. It rejects ancestor `.codex`, `.agents`
and AGENTS files, additional MCP servers and nonempty managed skillsets. Each
call creates its own owner-only HOME/CODEX_HOME outside the workspace, verifies
the pinned version strings and successful exits, opens one session, sends one
prompt, then kills the owned process group and waits for its leader. Stderr is
drained to discard rather than exposing unredacted diagnostics. Failed process
cleanup retains the invocation's private files instead of removing files a
process might still use.

The original runner/token and seat attribution remain attached to the platform
bridge. Only the model runtime's HOME/CODEX_HOME/TMPDIR are replaced. The real
workspace path remains in bootstrap context, while ACP and native tools start
in the neutral directory. Workspace AGENTS/config/skills discovery is therefore
intentionally different from the native coding adapter.

This helper does not establish a sandbox: callers must supply the isolated,
credential-free, non-root image and control system configuration, native skills,
runtime dependencies and egress. Ownership checks are not proof that arbitrary
host installations or their dependencies are trustworthy. Process groups are
not a container boundary for descendants that deliberately detach. Native ACP
permission denial is unchanged. Adversarial real-runtime configuration, tool,
cancellation and portable recovery fixtures remain prerequisites to production
registration; this change does not register or enable an adapter.


### Auxiliary model requests

Pinned ACP 2.5.0 normally starts asynchronous title and prompt-suggestion turns
using a hard-coded model. The plan disables prompt suggestions explicitly, and
the trusted session setup pins a fixed non-user title with the target-specific
`_session/rename` extension before any prompt. A failed rename closes the client.
This is required model-routing configuration, not cosmetic UI behavior. The real
fixture leaves the session alive after a turn and checks every observed model
request, so immediate process teardown cannot conceal auxiliary calls.
### Unregistered restricted lifecycle experiment (Linux only)

`NewRestricted(ExperimentalLauncher{...})` constructs a separate
`RestrictedAdapter`; it does not replace `New`/`Adapter`, add a registry entry,
change a runtime image, or enable a profile. The original disabled adapter's
methods still fail closed as described above. The restricted adapter is only
for an explicitly provisioned host satisfying the launcher's isolation contract.
Mock lifecycle tests are not evidence of production isolation.

- `Prepare` checks paths and the deterministic seat model/tool plan, owns a copy
  of mutable configuration, and rejects all harness config, extra MCP servers,
  nonempty managed skills and authorization errors. Static STEADMESH_EXECUTION
  and alternate STEADMESH_EXECUTION_FILE overrides are rejected: the bridge
  reads current attribution from the original runner-owned execution file.
  It launches nothing and does
  not claim executable-integrity verification. Runtime ownership/version checks
  remain in the launcher and run before each prompt.
- `StartOrResume` accepts only fresh or portable handoff recovery. Any supplied
  native checkpoint becomes a portable fallback, never a `loadSession` request.
  Explicit recovery replaces bootstrap recovery fields and retains unknown
  operations for reconciliation. Startup itself does not launch a process.
- Every `Deliver` invokes the launcher exactly once after refreshing managed
  skill authorization. The launcher rechecks authorization before the prompt.
  A new private HOME/CODEX_HOME and fresh native session are created each time.
  There is no implicit conversation history between deliveries: platform handoff,
  memory, message-history and operation tools are the durable context. The
  startup handoff is a snapshot, so the agent is told to recover current context
  using those tools. Bootstrap instructions, the original token/runner paths,
  execution attribution, and seat model forwarder remain authoritative.
- `TurnGuard` serializes delivery and owns interruption; `EventBus` owns the
  stable stream and closes only after the active delivery unwinds. Consumers
  must drain events. Cancellation takes precedence over a simultaneous terminal
  success. `end_turn` alone means technical completion; `cancelled` means
  interrupted; refusal and token/request limits mean failed. Tool updates are
  correlated progress observations, not assertions of tool success or authority.
  Raw launcher errors are not exposed as turn error strings.
- `Checkpoint` atomically writes an owner-only portable descriptor in RunnerDir
  with format `acp-portable-handoff-v1` and reference `none`. Its application
  guarantee covers this descriptor only; it does not persist a native session,
  transcript or process. The platform separately persists handoff updates. No
  process snapshot, native resume or graceful suspension is advertised.
- `Quiesce` waits for a turn boundary or interrupts at its deadline. `Stop`
  cancels the owned turn and waits for launcher teardown; an expired caller
  deadline may return before teardown finishes, but the event stream closes only
  after delivery finishes. The production launcher bounds cancellation and owns
  process-group cleanup. An injected test launcher must obey the same contract.

This mode intentionally starts in the immutable neutral cwd, not WorkspaceDir.
Workspace paths are explicit context only; native workspace instructions,
configuration and skills are not discovered. Read-only native tools and denied
ACP permissions do not confer workspace write access. The launcher restrictions
and remaining production promotion gates above still apply.

Offline tests cover lifecycle ordering, preparation without execution, copied
model authority, recovery/unknown-operation preservation, sequential fresh
launches, stream correlation and closure, stop-reason mapping, busy rejection,
quiesce/stop/caller cancellation, deadline precedence, skill refresh failure,
unsupported configuration, and portable checkpoint persistence. No credentials
or remote model calls are used by these tests.

The opt-in `TestExperimentalLauncherRestrictedLifecycle` uses the real wrapper
and is selected by the isolated image's existing test prefix. It checks that
Prepare/Start issue no model requests, readiness calls real `self`, delivery
context does not leak into the next fresh session, explicit portable handoff and
unknown operations reach the model, and checkpoint/stop/reconstruction use a
fresh session with current model/tool execution fencing. Event collectors verify
message/execution ownership through closure. Runtime-path absence skips this
test; compilation or a skipped run does not establish real lifecycle evidence.
