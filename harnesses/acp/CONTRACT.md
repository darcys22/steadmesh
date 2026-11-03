# ACP compatibility spike (disabled)

## Status and scope

This package is **not a production harness**. It is deliberately absent from
`harnesses/all` and the registry. `Prepare`, `StartOrResume`, `Deliver`, `Quiesce`
and `Checkpoint` fail closed. No process is spawned, no Environment is retained,
no credential is written, and no default adapter changes. There is no enable flag.
`DescribeCapabilities` advertises no operational tools, streaming, recovery,
checkpoint or suspension capabilities. `Stop` only closes the event channel.

The useful executable portion is an offline ACP v1 JSON-RPC wire decoder with
mocked exchanges. It binds output to a session and prompt, bounds record size,
normalizes text/tool observations, cancels every permission request, rejects
unsupported requests and updates, and requires a terminal prompt response.
It is intentionally strict and is not a complete ACP client. Tool observations
never execute tools. Permission replies are returned as bytes, never transmitted.
A decoder is single-use and single-threaded. EOF or any invalid record poisons it.
Callers must bound records *while reading*; the decoder cannot prevent an upstream
reader from allocating an oversized record. No streaming transport exists here.

## Why not wrap acpx and enable it?

The inspiration is [no-mistakes internal/agent/acpx.go](https://github.com/kunchenguid/no-mistakes/blob/11947ea77b4b8ab8c9402eac689d07b1bcb6ee26/internal/agent/acpx.go),
which launches `acpx` and parses JSON event output. That implementation's
`--approve-all` is **not adopted**. Its generic model selection and session
behavior do not demonstrate Steadmesh's stronger seat contract. This is an
independent implementation against [ACP v1 prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn)
and [session setup](https://agentclientprotocol.com/protocol/v1/session-setup),
not a claim that raw ACP and a particular acpx CLI output are interchangeable.
No acpx binary/version or target agent has been pinned or tested by this spike.

## Compatibility gates before a runnable adapter

Each gate needs evidence for an exact target/version, not caller-supplied booleans
or an agent's capability assertion alone. Keep the current built-in adapters
until the full boundary is proven.

| Boundary | Required evidence | Current limitation |
| --- | --- | --- |
| Model proxy | Exact model ID/API/settings and every request routed through `Environment.Model.BaseURL`; placeholder key only; test no native provider fallback | ACP model selection does not standardize Steadmesh endpoint configuration. No target-specific config mapping exists here. |
| Credentials and egress | Clean HOME/environment, platform token-file rotation, existing access-plugin proxy controls, no provider keys in the sandbox | Generic acpx invocation/inherited environment cannot establish isolation. This spike never launches it. |
| Permissions | A seat-authority-aware request bridge with explicit denied/unknown handling; no blanket approvals; audit correlation | Only offline cancelled replies exist. Filesystem, terminal and other agent requests are rejected. |
| Tools | Platform tool bridge plus authorized extra MCP servers, unchanged token/fencing authority and transport semantics; real readiness probe | ACP session/new supports MCP configuration, but initialization/transport/tool integration has not been implemented. Tool events are informational only. |
| Skills/instructions | Trusted bootstrap and per-turn `LoadSkills` refresh/revocation, fail closed on loader errors, correct native instruction precedence | No target-neutral mechanism proves skill revocation or prevents stale native caches. |
| Recovery | Durable seat-volume session state, format/version identity, config revision checks, unknown-operation reconciliation, explicit portable handoff | ACP `loadSession` alone is not a Steadmesh application checkpoint. No recovery guarantee is advertised. |
| Lifecycle | Serialized Deliver, concurrent Stop/Quiesce, bounded I/O, cancellation/timeout escalation, process-group cleanup and drained event ordering | The offline decoder does not own a process or claim operational interruption. |
| Protocol/runtime | Digest/version-pinned CLI and target, initialized protocol/capability negotiation, bounded framing, target-specific recorded conformance fixtures | Tests are mocked ACP v1 records. Numeric prompt IDs, non-text content and unknown update variants are deliberately unsupported. |

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
These tests are not real-agent conformance or a security audit.

Promotion requires a separate reviewed change: pin one real target, implement
and verify every boundary above with the real model forwarder and tool bridge,
then register it and add runtime image/profile support. Do not enable first and
silently downgrade missing capabilities later.
