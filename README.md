# Steadmesh

This platform turns an organisation declared in Terraform (culture, teams,
seats, harnesses, memory, grants, message routes and channel bindings) into
persistent agents running on Kubernetes. People message their personal
representatives over Slack. Representatives keep context, delegate to other
seats, and agents create work in Linear.

**Documentation:** open [`docs/index.html`](docs/index.html) in a browser. It is
a static site, with no build step, containing a quickstart, a tutorial, guides
and reference pages. The design is in
[`agent_organisation_initial_technical_design.md`](agent_organisation_initial_technical_design.md),
and acceptance status is tracked in
[`tests/acceptance/RESULTS.md`](tests/acceptance/RESULTS.md).

## Quickstart: run agents in your cluster

You don't need to clone this repository. Terraform downloads everything: the
provider from the Terraform Registry, the platform chart and images from
ghcr.io, and the modules from GitHub.

### What you need

- **A Kubernetes cluster** and a kubeconfig context for it. The cluster needs:
  - a CNI that enforces NetworkPolicy, because seats only start once their
    sandbox is verified (kind's default CNI, Calico and Cilium all qualify);
  - a default StorageClass;
  - about 4 CPUs and 8 GB of memory free.
- **Terraform** 1.5 or later, and **kubectl**.
- **A Slack app.** Create it from
  [`slack-app-manifest.yaml`](https://github.com/darcys22/steadmesh/blob/main/docs/assets/slack-app-manifest.yaml)
  (api.slack.com/apps → Create New App → From a manifest). You need:
  - an app-level token (`xapp-…`, scope `connections:write`);
  - the bot token (`xoxb-…`);
  - your workspace ID (`T…`);
  - the member ID (`U…`) of each person who gets a representative.
    In Slack: profile → ⋮ → Copy member ID.
- **An Anthropic API key.** Agents run Claude Code.
- *Optional:* a **Linear** API key and team ID, so agents can create projects
  and issues.

### 1. Get the quickstart

```sh
mkdir steadmesh && cd steadmesh
terraform init -from-module="github.com/darcys22/steadmesh//quickstart?ref=v0.1.0"
```

This copies two small Terraform roots into the directory:

- `platform/` installs Steadmesh into your cluster;
- `organisation/` declares your agents.

They are separate because the organisation's provider needs the platform's
CRDs to exist before it can plan.

### 2. Install the platform

```sh
cd platform
cp terraform.tfvars.example terraform.tfvars   # set kube_context and your tokens
terraform init
terraform apply
```

This creates the `steadmesh-system` and `steadmesh` namespaces, Postgres, the
platform chart and the credential Secrets. It never uses your current kubectl
context, only the `kube_context` you set. The tokens end up in this root's
Terraform state, so keep the state private.

### 3. Declare your organisation

```sh
cd ../organisation
cp terraform.tfvars.example terraform.tfvars   # workspace ID and the people
terraform init
terraform apply
```

Each person in `humans` gets a personal representative. Representatives can
delegate to an engineering lead, who works with an engineer. The organisation
stage reads the cluster, namespace and secret references from the platform
stage's state. `terraform apply` returns once every seat has passed its
readiness checks, which takes a few minutes the first time while the seat
images are pulled.

### 4. Talk to your representative

In Slack, open the app and send it a direct message. Your representative
replies in the same DM. Try:

- asking it to remember a preference;
- asking it to have engineering look into something;
- with Linear connected, asking for a project for a small task.

To watch the agents work, set `enable_console = true` in
`platform/terraform.tfvars`, apply, and open the read-only console:

```sh
kubectl --context <your-context> -n steadmesh-system port-forward deploy/steadmesh-console 8090
# http://127.0.0.1:8090
```

### Shape your organisation

Everything about your organisation is in `organisation/`:

- **Culture and roles:** edit `instructions/culture.md` (how everyone works)
  and `instructions/representative.md` (how representatives behave).
- **People:** add an entry to `humans` in `terraform.tfvars`. One Slack app
  serves every representative.
- **Seats:** add a teammate to `local.engineering` in `main.tf`, for example
  `reviewer = { role = "reviewer", display_name = "Reviewer" }`. Then give
  it a route under `message_routes`, such as
  `eng_lead_reviewer = { from = "seat:eng_lead", to = "seat:reviewer", bidirectional = true }`.
  Seats can only message along declared routes.
- **Access:** connections are only usable through `grants`. The Linear grant
  gives the whole engineering team `project.create` and `task.write`.

`terraform plan` shows exactly which seats a change affects. Seats keep their
identity, workspace and memory across applies. The
[tutorial](docs/tutorial.html) walks through every part of the declaration.

### Upgrade or remove

- **Upgrade:** fetch the new release's quickstart into a fresh directory and
  carry over your `terraform.tfvars`, `instructions/` and state. Or change
  every `?ref=v…`, `steadmesh_version` and the provider `version` in your copy.
  Then apply `platform/`, then `organisation/`.
- **Remove:** run `terraform destroy` in `organisation/`, then in `platform/`.
  With `data_retention = "retain"`, destroying the organisation keeps its
  memory and history in the platform's database. Destroying `platform/`
  deletes the namespaces, and with them that database.

## Develop from source

Prerequisites:
- Go 1.27
- Docker (Colima or Docker Desktop)
- kind
- kubectl
- helm
- Terraform ≥ 1.5

Every cluster target pins the `kind-steadmesh` context.

```sh
make tools              # controller-gen, setup-envtest into ./bin
make test               # unit tests
make test-integration   # Postgres (testcontainers), envtest, Terraform acceptance
make e2e                # fresh kind cluster, local images, full apply-to-conversation suite with fakes
make quickstart-test    # fresh kind cluster, runs quickstart/ as a user would, with local builds
```

`make e2e` and `make quickstart-test` delete and recreate the kind cluster,
and remove the example Terraform state, before they run.

To deploy the in-repo example organisation (`examples/`) from source and talk
to it through fake Slack and Linear servers, follow
[`docs/from-source.html`](docs/from-source.html). To use real Slack, Linear
and Claude Code with that setup, see
[`docs/real-services.html`](docs/real-services.html) and
[`tests/live/README.md`](tests/live/README.md).

## Releasing

Pushing a tag `vX.Y.Z` runs [`.github/workflows/release.yml`](.github/workflows/release.yml),
which publishes:

- **images:** `ghcr.io/darcys22/steadmesh/<image>:X.Y.Z`, for amd64 and arm64;
- **chart:** `oci://ghcr.io/darcys22/steadmesh/charts/platform`, version `X.Y.Z`;
- **provider:** signed builds and their Registry documentation on
  [darcys22/terraform-provider-steadmesh](https://github.com/darcys22/terraform-provider-steadmesh),
  which the Terraform Registry serves as `darcys22/steadmesh`;
- **a GitHub release** in this repository.

To cut a release, run `make release` on a clean, up-to-date `main`. It bumps the
minor version from the latest tag (`BUMP=patch` or `BUMP=major`, or
`VERSION=X.Y.Z`), runs `make lint test`, then does what you could do by hand:

```sh
hack/set-version.sh X.Y.Z    # point quickstart/ and the provider doc examples at the new tag
make provider-docs           # re-render provider/docs
git commit -am "Release vX.Y.Z" && git tag vX.Y.Z && git push origin main vX.Y.Z
```

It asks before pushing (`YES=1` skips the question); answering no leaves the
release commit and tag local, with the commands to push or undo them.

The workflow refuses a tag that `quickstart/` doesn't pin, and fails if
`provider/docs` is out of date. The provider documentation is rendered by
`make provider-docs` from the schema's descriptions, `provider/docs-templates`
and `provider/docs-examples`, so a change to the provider schema needs a
`make provider-docs` too.

One-time setup:

1. **Provider release repository.** Create the public repository
   `darcys22/terraform-provider-steadmesh` with at least one commit (a README
   is enough). The Terraform Registry only reads releases from a repository
   with that name.
2. **Signing key.** Generate a GPG key (RSA, without an expiry or with a long
   one).
   - Add its ASCII-armoured private key and passphrase to this repository's
     secrets as `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE`.
   - Add a fine-grained token with *Contents: read and write* on the provider
     repository as `PROVIDER_RELEASE_TOKEN`.
3. **Registry.** After the first release, sign in to
   [registry.terraform.io](https://registry.terraform.io) with GitHub. Publish
   the provider from `darcys22/terraform-provider-steadmesh`, and add the GPG
   public key under your namespace's signing keys.
4. **Package visibility.** After the first release, make the ghcr.io packages
   public: the five images and `charts/platform`. Do this under the
   repository's Packages → Package settings.

## Components

| Path | What it is |
| --- | --- |
| `quickstart/` | Bare-bones install from the published release: `platform/` and `organisation/` Terraform roots |
| `pkg/spec`, `pkg/compile` | Organisation schema and the single validator/compiler used by both the provider and the controller |
| `api/v1alpha1` | The `AgentOrganization` and controller-owned `AgentSeat` CRDs |
| `provider/` | Terraform provider `steadmesh` (`steadmesh_organization`) |
| `controller/`, `runtime/` | Reconciler, seat lifecycle (wake, idle stop, fenced recovery, retirement) and Kubernetes sandbox backend |
| `services/` | Trusted platform service: identity, inbox/outbox, memory, tool gateway, operation ledger, scheduler, model proxy, console API |
| `connectors/` | Slack (Socket Mode), Linear, Anthropic model proxy, Vault/Kubernetes secret resolvers |
| `harnesses/`, `cmd/seat-runner`, `cmd/steadmesh-tools` | In-Pod seat supervisor, the Claude Code and fake harness adapters, and the MCP/CLI tool client |
| `console/`, `cmd/console` | The optional read-only Steadmesh Console |
| `charts/platform` | Helm chart: CRDs, controller, platform service and console |
| `modules/`, `bundles/` | Terraform modules (platform install, Postgres, team templates, representatives) and instruction bundles |
| `examples/` | Foundation → platform → organisation deployment roots built from source, and `make plan/apply/status/teardown` |
| `cmd/orgctl` | Status, fresh readiness verification and `orgctl console` |

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
