# Example deployment

Three Terraform roots, applied in order (design §5.3):

| Stage | Root | Creates |
| --- | --- | --- |
| foundation | `foundation/` | Namespaces `steadmesh-system` and `steadmesh-example`; Postgres (StatefulSet, Service, Secret `steadmesh-db` with key `url`); optional Vault in dev mode; credential Secrets |
| platform | `platform/` | `helm_release` of `../../charts/platform` (CRDs, controller, platform service), waited until available |
| organisation | `organisation/` | Instruction bundle ConfigMaps and one `steadmesh_organization`: two representatives bound to two Slack users, plus an engineering team (lead, engineer, reviewer) |

Everything targets the kind cluster `steadmesh` through the context
`kind-steadmesh`. No root, Makefile target or tool here reads the ambient
kubectl context.

## Prerequisites

- **Docker Desktop**, running.
- **kind** v0.24 or later (`brew install kind`). Its kindnet CNI enforces
  NetworkPolicy, and the controller verifies that before it reports
  `SandboxEnforced`.
- **kubectl** v1.30 or later.
- **Terraform** v1.5 or later (tested with 1.5.7). `terraform init` downloads
  `hashicorp/kubernetes ~> 2.38`, `hashicorp/helm ~> 2.17` and
  `hashicorp/random ~> 3.6`, plus the `hashicorp/vault` chart (0.30.0) when
  `enable_vault = true`. All of these need network access.
- **Go 1.27**, to build the provider (`bin/terraform-provider-steadmesh`) and
  `bin/orgctl`. From the repository root, run `make build`.
- **Images loaded into kind.** From the repository root, run
  `make kind-up kind-load TAG=dev`. This builds and loads
  `steadmesh/{controller,platform,seat-fake,seat-claudecode}:dev`.
- **GNU Make.** The macOS `make` (3.81) works.

For the deterministic path (`harness = "fake"` with the in-repo fake Slack
and Linear servers), nothing else is needed. Point `slack_endpoint_ref` and
`linear_endpoint_ref` at the fakes. The credential defaults in `foundation/`
are placeholders that the fakes accept.

For a live run, you also need:

| Item | Where it goes |
| --- | --- |
| A Slack app installed from `docs/assets/slack-app-manifest.yaml` (Socket Mode): bot token `xoxb-…` and app-level token `xapp-…` | `TF_VAR_slack_bot_token`, `TF_VAR_slack_app_token` (foundation) |
| The Slack workspace ID and two Slack user IDs | `slack_workspace_id`, `humans` (organisation) |
| A Linear API key and team ID | `TF_VAR_linear_api_key` (foundation), `linear_team_id` (organisation) |
| An Anthropic API key, when `harness = "claude-code"` | `TF_VAR_anthropic_api_key` (foundation) |

## Usage

```sh
cd examples
make apply      # foundation -> platform -> organisation -> orgctl verify
make status     # kubectl get agentorganizations,agentseats -A; orgctl status
make plan       # plan all stages (on a bootstrapped cluster)
make teardown   # destroy organisation, platform, foundation (the kind cluster stays)
make validate   # terraform validate every root; needs no cluster
make plan-envtest  # plan every root against a temporary envtest API server
```

`make apply` creates the kind cluster if it is absent. It then plans and
applies each stage in order and finishes with `bin/orgctl verify`, a fresh
readiness check that runs even when Terraform had nothing to change (§5.4,
A25). Each stage is a separate target (`apply-foundation`, `apply-platform`,
`apply-organisation`). Every recipe runs under `set -euo pipefail`, so the
first failing stage stops the run with its exit code.

Artifacts in `out/`:

| File | Contents |
| --- | --- |
| `<stage>.tfplan` | Saved plan that was applied |
| `<stage>.plan.txt` | Rendered plan |
| `<stage>.{init,plan,apply,destroy,validate}.log` | Command logs |
| `<stage>.outputs.txt` | Outputs |
| `verify.log`, `status.*.log` | Readiness and status |

`make plan` plans every stage. On a fresh cluster, the platform and
organisation plans fail until the earlier stages have been applied. The
organisation provider fails early when the `steadmesh.io/v1alpha1` API is not
served. This is intentional: one undifferentiated first apply is not
supported (§5.3).

### The locally built provider

The Makefile writes `out/terraformrc` and exports
`TF_CLI_CONFIG_FILE=out/terraformrc`. `~/.terraformrc` is never read or
written. The generated config contains:

- a `dev_overrides` entry for `darcys22/steadmesh`, pointing at `../bin`, which
  selects the binary that runs;
- a filesystem mirror at `out/mirror` serving the same binary, because
  Terraform 1.5 `terraform init` still resolves overridden providers.

Terraform prints a "Provider development overrides are in effect" warning;
this is expected. To use the provider by hand:

```sh
make tfrc mirror
export TF_CLI_CONFIG_FILE=$PWD/out/terraformrc
terraform -chdir=organisation init && terraform -chdir=organisation plan
```

## Organisation configuration

`organisation/` composes the modules:

- `modules/instruction-bundle` publishes `bundles/culture/culture.md` and
  `bundles/roles/representative.md` as ConfigMaps in the organisation
  namespace. Each one is referenced as
  `configmap:<name>/<key>#sha256:<digest>`.
- `modules/team-base` and `modules/team-engineering` produce the `base` and
  `engineering` team templates. `engineering` extends `base`, and its roles
  are `engineering_lead`, `engineer` and `reviewer`. The team `engineering`
  instantiates the template, and its seats select roles with
  `role_ref = "role:<key>"`. The provider resolves templates, so the plan
  shows each seat's resolved role and configuration revision in
  `resolved_seats`.
- `modules/representative` (one per entry in `var.humans`) produces the
  seat, its private memory store and the Slack channel binding.

| Item | Configuration |
| --- | --- |
| Memory | Stores `organisation`, `engineering`, and one personal store per seat. Every seat can read and search `organisation`. Engineering members get read/search/write/revise/archive/history on `engineering` through the template. |
| Grants | `team:engineering` gets `project.create` and `task.write` on the `linear` connection. |
| Routes | Each representative can message `eng_lead`, which can reply. `eng_lead` and `engineer`, and `eng_lead` and `reviewer`, can message each other both ways. |
| Harness | `-var harness=claude-code` (adds the `model` connection with adapter `anthropic`) or `-var harness=fake` (the default). |
| External endpoints | `slack_endpoint_ref`, `linear_endpoint_ref` and `model_endpoint_ref` override base URLs, for example to point at the fakes. Leave them empty to use the real services. |

Credentials are never in this root. Connections carry only references
(`secret_ref = "k8s:slack-credentials"`). The foundation stage creates those
Secrets in `steadmesh-system` from sensitive variables, and only the platform
ServiceAccount can read them. The values therefore never enter the
organisation root's state or plans (A23).

They **do** enter the foundation root's state, because the kubernetes
provider stores Secret data there. Protect that state, or create the Secrets
out of band and keep the placeholder defaults. `sensitive = true` only hides
values from CLI output; it is not a storage boundary (§10.2).

## Readiness, timeouts and recovery

The provider waits for `OperationalReady=True` on the current generation
(`wait_for_ready = true`, default timeout 20m). If readiness times out:

- The apply fails with the blocking condition's reason and message.
- The resource stays in state, and nothing is cleaned up.
- On **update**, fix the cause and apply again.
- On **create**, Terraform core marks the resource *tainted*. Run
  `terraform -chdir=organisation untaint steadmesh_organization.this` to keep
  the organisation; otherwise the next apply replaces it.

Importing an existing organisation:

```sh
terraform -chdir=organisation import steadmesh_organization.this steadmesh-example/example-company
```

Objects annotated `steadmesh.io/managed-by=helm` are rejected. An object with
no owner annotation needs explicit adoption: use the ID
`steadmesh-example/example-company/adopt`. `wait_for_ready` and `timeouts` are
not stored on the object, so an import assumes their defaults.
