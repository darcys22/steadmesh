# Live acceptance tests

These tests run the real path: Slack, Linear, the Claude Code harness and an
Anthropic model, on the local kind cluster. Design §17 requires at least one
real end-to-end conversation before the platform is called usable. Results
from the fake-backed e2e suite are not enough.

## Prerequisites

1. Complete the Slack and Linear setup in
   [Connect real services](../../docs/real-services.html),
   including installing the app from `docs/assets/slack-app-manifest.yaml`.
2. Export the following variables.

| Variable | Meaning |
| --- | --- |
| `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` | App tokens (`xoxb-…`, `xapp-…`) |
| `SLACK_TEAM_ID` | Workspace ID (`T…`) |
| `SLACK_USER_SEAN`, `SLACK_USER_ALEX` | Member IDs of two test humans |
| `SLACK_USER_TOKEN_SEAN`, `SLACK_USER_TOKEN_ALEX` | User tokens (`xoxp-…`, scope `chat:write`, `im:history`) so the test can message the bot as those humans. Use test accounts. |
| `LINEAR_API_KEY`, `LINEAR_TEAM_ID` | Linear credentials. Use a dedicated test team: the test creates a project there. |
| `ANTHROPIC_API_KEY` | Model credential, kept in the control plane. Seats never see it. |

3. Run `make live`.

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
