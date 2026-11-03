# Safe harness fallback foundation

Steadmesh seats authorize a single adapter and model connection. Their native
session and checkpoint belong to that adapter. Unlike a stateless CLI fallback
chain, changing either at runtime needs platform authorization and explicit
lifecycle management. The original primitive does not itself enable cross-model or cross-adapter
failover. The opt-in [startup fallback integration](startup-fallback.md) now
supports approved missing-executable fallback before any session or turn starts.

The seat runner now records per-attempt operational telemetry for the existing
single authorized adapter. `harnesses.RunApprovedFallback` provides an opt-in,
ordered primitive for callers that already own authorization and lifecycle:

- Default policy makes one attempt. Maximum chain and attempt count are eight.
- Multiple attempts require a shared deadline; parent cancellation, quiesce and
  stop errors never cause a fallback. The timeout is never reset per attempt.
- Only structured `OperationalError` classifications for unavailable execution
  and quota exhaustion can opt into fallback. Authentication, policy,
  configuration, session, invalid output and unknown failures are terminal.
- The caller must prove the failed attempt was never executed. Quota errors,
  crashes and missing responses are not that proof. A started tool-capable turn
  with uncertain side effects must not be replayed.
- Any output, session, recovery or business-claim metadata vetoes replay even
  when the caller asserts non-execution. Business verdicts never choose routes.
- Each attempted candidate emits a report without prompt/output or provider
  error contents. Candidates are attempted at most once, in configured order.

Callbacks must honor cancellation and return only after the invocation stops.
This synchronous helper intentionally does not abandon running goroutines,
create adapters, handle credentials, transfer native sessions or bypass the
model proxy. Callers must not pass model-authored candidate lists or replay
safety assertions.

## Wider seat failover

To enable alternative adapters/models, add a platform-owned manifest contract
that approves every candidate endpoint and adapter, validate proxy model grants
for that set, and implement the seat runner's stop/prepare/start/checkpoint and
event-stream ownership transitions. Native checkpoints must stay adapter-owned;
any portable handoff must be identified as a new session. Adapters must obtain
non-execution proof from trusted launch/protocol evidence, not error prose.
The startup-only integration implements explicit candidates, exact grants,
shared deadlines and durable checkpoint ownership. Mid-turn replay and
controller-driven image transitions remain unsupported; see the secure design
plan for the remaining requirements.

Inspired by no-mistakes' ordered, bounded operational fallback and per-attempt
reporting; implemented independently around Steadmesh's seat security model.
