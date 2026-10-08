# Mixed-harness organisation (completion demo)

An engineering team on three harnesses and three model endpoints, declared
with the example stages (`../foundation`, `../platform`, `../organisation`):

| Seat | Harness | Model endpoint | API |
| --- | --- | --- | --- |
| `eng_lead` | Claude Code | Anthropic (`claude-sonnet-5-5`) | `anthropic_messages` |
| `engineer` | Codex | OpenAI (`gpt-5.5`) | `openai_responses` |
| `reviewer` | Pi | selfhosted (`qwen3.8-27b`, `https://llm.example.com/v1`) | `openai_chat`, claimed by the connection and checked at readiness |

Only the engineer has GitHub access (access profile `github_engineer`). The
representatives use the deterministic fake harness, so the demo can drive
the team and check the results exactly. The team shares the `engineering`
memory store and the routes of `../organisation`.

The files are Terraform variable values for those stages:

- `fakes.json`: every model endpoint is the in-cluster scripted model fake and
  GitHub is the fake GitHub. Runs anywhere, with no credentials.
- `live.json.example`: the real Anthropic and OpenAI endpoints, a self-hosted
  OpenAI-compatible endpoint, and a real GitHub repository, which the
  engineer uses with git and gh (sandbox delivery through the egress
  gateway). Slack and Linear stay fakes.

## Run the demo

```
make demo                    # fakes
make demo DEMO_MODE=live     # real services
```

Each run starts a fresh kind cluster, applies the stages with the file's
values, and then:

1. has the team collaborate through messages and a shared note, with no
   Linear and no work items;
2. does the same with work items: the engineer claims one, opens a pull
   request, the reviewer reviews, the engineer completes it;
3. publishes work items to Linear (the fake), takes Linear down (the
   organisation stays ready, `IntegrationsDegraded` reports it), creates
   another item, reconnects and checks nothing is published twice;
4. rotates the Anthropic key in its Kubernetes Secret mid-session and checks
   the lead keeps working with no Terraform run and no restart.

It records the pinned harness versions, the configuration, each step's
result and evidence, and the model, API and upstream host of every model
request each seat made (from the platform's `model_request` events) in
`docs/demo/<mode>-results.json`, and renders `docs/demo-results.html` from
every recorded run. In fakes mode every model turn is scripted, so the run
proves the wiring, not model quality.

## Credentials for the live demo

Export these in your shell, or put them in `.env` at the repository root (ignored by git; the make targets source it). Never commit them.

| Variable | Use |
| --- | --- |
| `ANTHROPIC_API_KEY` | Anthropic Messages for the lead |
| `ANTHROPIC_API_KEY_2` | A second Anthropic key; step 4 rotates to it (skipped without it) |
| `OPENAI_API_KEY` | OpenAI Responses for the engineer |
| `SELFHOSTED_API_KEY` | The self-hosted endpoint for the reviewer |
| `SELFHOSTED_BASE_URL` | Its OpenAI-compatible API base, e.g. `https://llm.example.com/v1` |
| `GITHUB_TOKEN` | Fine-grained PAT with contents and pull requests read/write on the test repository |
| `GITHUB_TEST_REPO` | `owner/name` of a dedicated test repository |

The keys reach the cluster only as Kubernetes Secrets created by the
foundation stage; Terraform state of that stage holds them, so keep it
private (or create the Secrets yourself, see `docs/operations.html`).

## Use it without the demo

Pass the values to the stages yourself, for example:

```
cd examples/organisation
terraform apply -var-file=../mixed-harness/fakes.json
```

(`model_api_keys` belongs to the foundation stage and `enable_egress` to the
platform stage; Terraform warns about values a stage does not declare.)
