# Live acceptance tests

These tests run the real path: Slack, Linear, the Claude Code harness and an
Anthropic model, on the local kind cluster. Design §17 requires at least one
real end-to-end conversation before the platform is called usable. Results
from the fake-backed e2e suite are not enough.

## Prerequisites

1. Complete the Slack and Linear setup in
   [Connect real services](../../docs/real-services.html),
   including installing the app from `docs/assets/slack-app-manifest.yaml`.
2. Export the following variables, or put them in `.env` at the repository root (ignored by git; the make targets source it).

| Variable | Meaning |
| --- | --- |
| `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` | App tokens (`xoxb-…`, `xapp-…`) |
| `SLACK_TEAM_ID` | Workspace ID (`T…`) |
| `SLACK_USER_SEAN`, `SLACK_USER_ALEX` | Member IDs of two test humans |
| `SLACK_USER_TOKEN_SEAN`, `SLACK_USER_TOKEN_ALEX` | User tokens (`xoxp-…`, scope `chat:write`, `im:history`) so the test can message the bot as those humans. Use test accounts. |
| `LINEAR_API_KEY`, `LINEAR_TEAM_ID` | Linear credentials. Use a dedicated test team: the test creates a project there. |
| `ANTHROPIC_API_KEY` | Model credential, kept in the control plane. Seats never see it. `ANTHROPIC_MODEL` picks the model (default `claude-haiku-4-5-20251001`). |

3. Run `make live`.

## Harnesses on real model endpoints

`make live-harnesses` needs no cluster or Slack. It fetches the pinned
harness CLIs (`build/harness-bins.sh`) and runs one turn per harness and
endpoint, through the real model connector and forwarder, in which the model
calls the platform `self` tool over MCP. Each case runs when its credential
is set:

| Variable | Cases |
| --- | --- |
| `ANTHROPIC_API_KEY` (`ANTHROPIC_MODEL`) | claude-code and pi on Anthropic Messages |
| `OPENAI_API_KEY` (`OPENAI_MODEL`, default `gpt-5-mini`) | codex and pi on OpenAI Responses |
| `SELFHOSTED_API_KEY` (`SELFHOSTED_BASE_URL`, required, e.g. `https://llm.example.com/v1`; `SELFHOSTED_MODEL`, default `qwen3.8-27b`) | pi on OpenAI Chat Completions |

Each case first checks the endpoint as readiness does (a minimal request for
the API and model), then the turn. The log names the model, API and host
each request went to.

## Do models use the automation tools well?

`make live-automations` measures the scheduling interface with real models.
Each harness (claude-code and pi with `ANTHROPIC_API_KEY`, codex with
`OPENAI_API_KEY`) plays a representative whose human, in Melbourne, asks in
turn: remind me in 20 minutes; every weekday at 9am check the team and only
tell me if something needs attention; move that check to 10am; pause it;
resume it; cancel the reminder. It runs against a real platform (tools,
store and scheduler, with Postgres from testcontainers; needs Docker) and
fails when the automations left behind are wrong: a duplicate, a lost
weekday, the wrong time zone or a missed pause. The log shows each turn's
tool calls, reply and automations, and how many turns answered through
`messages.reply`.

The test applies the example stages with `harness=claude-code`. It checks the
following, then leaves the organisation running for inspection
(`make -C examples teardown` removes it):
- verification passes (A01)
- each human gets a reply from their own representative (A04, A05)
- a request is delegated to the engineering lead, which creates a Linear
  project that the test reads back (A06, A21)
- the representative remembers a stated preference after its Pod is killed
  (A07, A08)

Model output is non-deterministic, so the behavioural checks are
deliberately loose (§9.4). The test checks that the expected effects happened,
not the exact wording.

## GitHub

`make live-github` runs the github connector and the sandbox credential flow
against a real repository: the delivered credential lets git push a branch,
`pull_request.create` (platform delivery) opens a pull request that is found
again by its marker, and `gh` (when installed) sees it. The branch and pull
request are closed and deleted afterwards. Use a dedicated test repository.

| Variable | Meaning |
| --- | --- |
| `GITHUB_TEST_REPO` | `owner/name` of the test repository |
| `GITHUB_TOKEN` | Fine-grained personal access token with contents and pull requests read/write on that repository only |
