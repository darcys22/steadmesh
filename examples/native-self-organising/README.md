# native-self-organising

Every team uses peer coordination. A declared peer is the human terminal contact, not a privileged leader; it has the same board and routing policy as the pool. The native pool keeps 3 members available and admits at most 12. All delivery members may edit the mission.

This is a complete organisation-stage Terraform root for the **development
source tree**, not a claim that a release containing this feature is published.
Use matching locally built provider, platform/controller and seat images.

## Run locally

Follow [the common prerequisites](../README.md): Docker, kind, kubectl,
Terraform and Go. No live model credentials or external service accounts are
needed. The fake harness is deterministic and runs no model inference.
Initial provider/image downloads require network access; subsequent runs can
use cached artifacts offline.

From the repository root:

```sh
make build provider
make kind-up kind-load TAG=dev
cd examples
# This creates a local terminal credential, not a live external-service secret.
# Protect foundation Terraform state: it contains the generated local token.
TF_VAR_terminal_users='["operator"]' make apply-platform
make mirror
export TF_CLI_CONFIG_FILE="$PWD/out/terraformrc"
terraform -chdir=native-self-organising init
terraform -chdir=native-self-organising plan
terraform -chdir=native-self-organising apply
../bin/orgctl verify --context kind-steadmesh --namespace steadmesh-example --name native-self-organising
../bin/orgctl chat --context kind-steadmesh --namespace steadmesh-example --name native-self-organising --user operator
```

The root always selects `kind-steadmesh`, never the ambient kubectl context.
The foundation creates the namespace and local terminal fixture; the platform
stage installs CRDs and services. It does not apply the older `organisation/`
example. Multiple native roots may coexist because their organisation and
bundle names differ; `--name` selects the intended organisation.

Paste the line in `smoke.txt` into the chat. The expected reply is
`Native team fixture is running`. This tests terminal delivery and an ordinary
seat turn, not autonomous planning. The fake harness understands explicit
`/tool NAME JSON` commands; real model-led coordination needs a separately
configured real harness and its explicitly authorized connection.

To inspect peers through the same authenticated tool path, paste the two lines
in `discover.txt` as a single multiline chat message (Alt+Enter inserts a line).
Discovery reports route visibility; it does not change membership. You can
also call `/tool teams.pool {"team":"delivery"}` and then `/report` in one turn.

Inspect declared and runtime seats with:

```sh
kubectl --context kind-steadmesh -n steadmesh-example get agentorganizations,agentseats
```

For schema-only checking, `make validate-native` in `examples/` initializes
providers and validates all three roots without a cluster. A plan/apply needs
a running API server and current CRDs. No live cluster apply is implied by a
successful schema check.

## Boundaries and lifecycle

- `model_budgets` caps organisation and execution output tokens; the fake
  harness consumes no model tokens
- Pool `min_members` is available capacity, not continuous model activity
- Runtime participants have independent identities, inboxes, ordinary turns,
  handoffs and private memory; static personal stores are declared separately
- `seat` templates reuse ordinary harness/execution/sandbox profiles and
  explicit team board grants. No live connections, egress or hidden inherited
  requester capabilities are added
- The self-route explicitly allows teammate messaging; membership or mission
  editor status alone grants no route
- Review is required before accepted work; role text requests a distinct
  participant's review. Tool authority still comes from declared policy
- Change `pool.draining` to true and apply to stop admission and drain members.
  Handoffs, history and private memory are retained indefinitely because
  `retention_seconds = 0`. A positive value explicitly enables automatic
  cleanup of retired members’ private context, memory, session and handoff
  after that interval; shared evidence and message audit remain retained
- Destroying this organisation is not equivalent to wiping retained data:
  `data_retention = "retain"` and store retention are explicit

Clean up only this example with:

```sh
terraform -chdir=native-self-organising destroy
```

[Native team guide](../../docs/native-teams.html) explains context revisions,
authority, defaults and the distinction from legacy `tasks` / `elastic_teams`.
Neither legacy policies nor task history are automatically migrated.
