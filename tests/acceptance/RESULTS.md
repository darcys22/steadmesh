# Acceptance results (design §17)

Recorded 5 October 2026 at commit `HEAD`. Status values:

- **pass**: proven end to end on kind (`make e2e`, fake harness and fake Slack/Linear) and by the integration suites.
- **pass (integration)**: proven by integration tests against real Postgres, envtest or Terraform, but not by the kind e2e.
- **partial**: the mechanism is implemented and partly tested; the gap is stated.
- **pending-live**: needs the real-credential run (`make live`, see `tests/live/README.md`). §17 does not allow the platform to be called usable on mocked connectors alone. No `make live` run has been recorded yet.
- **deferred**: Milestone 5 or 6 work.

| ID | Scenario | Status | Evidence |
| --- | --- | --- | --- |
| A01 | Apply prepared configuration | pass; pending-live | e2e `A01`: `make -C examples apply` ends with `orgctl verify` OperationalReady=True and representatives in the outputs |
| A02 | Connector lacks authorisation | pass (integration) | provider acc test: a blocked update reports `ConnectionsAuthenticated` and its message; controller envtest: a failing required connection blocks readiness |
| A03 | Repeat apply | pass | e2e `A03` (`plan -detailed-exitcode` = 0, 5 seats); provider acc no-op plan; controller envtest second reconcile is a no-op |
| A04 | First human message | pass; pending-live | e2e `A04`: Slack DM → representative → reply, with no manual runtime command |
| A05 | Two representatives | pass; pending-live | e2e `A05`: Alex's history does not include Sean's conversation, and forged text is ignored; services tests check binding by verified ids |
| A06 | Delegate to another seat | pass; pending-live | e2e `A06`: rep → eng_lead `/task` → correlated result forwarded to the human |
| A07 | Contact a sleeping seat | pass | e2e `A07`: seat scales to 0 after idle timeout; a message wakes it and the workspace file is intact |
| A08 | Kill a running Pod | pass | e2e `A08`: same seat ID, workspace intact, lease generation advanced; services fencing tests reject stale generations |
| A09 | Replay inbound event | pass | e2e `A09`: same Slack `event_id` twice → one reply; services dedupe tests |
| A10 | Lose external response | pass | e2e `A10`: fake Linear commits then drops the response; the gateway reads back the marker and exactly one project exists; services unit tests cover the `unknown` path |
| A11 | Concurrent shared-memory edit | pass (integration) | store and HTTP tests: a stale revision conflicts and both revisions stay attributed |
| A12 | Search inaccessible memory | pass | e2e `A05` memory search returns nothing from another rep's store; store and HTTP no-leak tests |
| A13 | Remove membership or grant | partial | HTTP test: denied on the very next tool call. The quiesce of live execution on a revision change is covered only by controller lifecycle unit tests, not e2e |
| A14 | Change role or culture | partial | compile test: only affected seats change revision; executions record the config revision. No e2e of a live instruction change |
| A15 | Replace a harness | partial | conformance: a foreign or missing session falls back to the portable handoff with an explicit note. A full fake → claude-code migration of one seat has not been run |
| A16 | Unsupported sandbox feature | pass (integration) | compile fixtures reject `suspend` and `microvm` without a runtime class; controller envtest: missing RuntimeClass → Blocked, no StatefulSet |
| A17 | Management API from a seat | pass | e2e `A17`: the seat SA cannot create pods, patch orgs, create rolebindings or get secrets; no token automount; token audience `steadmesh-gateway` only |
| A18 | Retire and recreate a seat | pass (integration) | store tests: a recreated key gets no private data; the controller reuses a retained PVC only with `adopt_from` |
| A19 | Destroy with retention | pass (integration) | controller envtest: delete with retain keeps PVCs and calls the platform with `retention=retain` |
| A20 | Restart control plane | pass | e2e `A20`: controller and platform restarted; still 5 seats, new messages answered, no duplicate projects |
| A21 | Agent creates a project | pass (fake tracker); pending-live | e2e `A06`: project created through `connections.invoke` and visible in the tracker; no Terraform record |
| A22 | Request authority in natural language | partial | Authority comes only from the compiled manifest and is enforced server-side (policy tests); not exercised with a real model |
| A23 | No raw credentials | pass | e2e `A23`: no token values in organisation/platform plans, organisation state or runtime objects. Foundation state does hold them (documented in examples/README) |
| A24 | Config change through CI | not run | The CI path is the same `make apply` workflow; no CI system is wired in this repository |
| A25 | No-change apply still verifies | pass | e2e `A25`: with the fakes scaled to zero, `orgctl verify` fails; after recovery it passes |
| A26 | Restore from backup | deferred (M5) | ADR-0006 |
| A27 | Background vs interactive load | deferred (M6) | ADR-0006; no DSec features are claimed |
| A28 | Retire a busy seat (added with seat retirement; not in the design's original 27) | pass | e2e `A28` (`tests/e2e/retirement_test.go`): the running turn finishes, the seat saves a handoff on its retirement notice, its work item is released, a queued message goes back to its sender, both representatives get a summary, and the workspace is kept |

Real-services evidence outside `make live`:

- The completion demo (`make demo DEMO_MODE=live`, `examples/mixed-harness`)
  recorded a run on 8 October 2026 with real model endpoints (Claude Code on
  Anthropic Messages, Codex on OpenAI Responses, Pi on an OpenAI Chat
  Completions endpoint) and a real GitHub repository; Slack and Linear were the
  in-cluster fakes. Readiness, collaboration, work items with a pull request
  (left `in_review`) and publishing through a Linear outage passed; the key
  rotation step was skipped. Record: `docs/demo/live-results.json`, rendered
  at `docs/demo-results.html`. It does not change any scenario above to a
  real-services pass, because the scenarios marked pending-live need real
  Slack and Linear.

Commands and results:

- `make test`: all packages pass.
- `make test-integration`: all packages pass (testcontainers Postgres, envtest 1.37, Terraform 1.5.7, real Claude Code 2.1.289 against a stub model API).
- `make e2e`: `ok github.com/darcys22/steadmesh/tests/e2e 213.550s`, on a freshly created kind cluster (after the Steadmesh rename).
