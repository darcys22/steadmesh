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

## Components

| Path | What it is |
| --- | --- |
| `pkg/spec`, `pkg/compile` | Organisation schema and the single validator/compiler used by both the provider and the controller |
| `api/v1alpha1` | The `AgentOrganization` and controller-owned `AgentSeat` CRDs |
| `provider/` | Terraform provider `steadmesh` (`steadmesh_organization`) |
| `controller/`, `runtime/` | Reconciler, seat lifecycle (wake, idle stop, fenced recovery, retirement) and Kubernetes sandbox backend |
| `services/` | Trusted platform service: identity, inbox/outbox, memory, tool gateway, operation ledger, scheduler, model proxy |
| `connectors/` | Slack (Socket Mode), Linear, Anthropic model proxy, Vault/Kubernetes secret resolvers |
| `harnesses/`, `cmd/seat-runner`, `cmd/steadmesh-tools` | In-Pod seat supervisor, the Claude Code and fake harness adapters, and the MCP/CLI tool client |
| `charts/platform` | Helm chart: CRDs, controller and platform service |
| `modules/`, `bundles/` | Terraform team templates and instruction bundles |
| `examples/` | Foundation → platform → organisation deployment roots and `make plan/apply/status/teardown` |
| `cmd/orgctl` | Status and fresh readiness verification |

## Quick start (local kind cluster, deterministic fakes)

Prerequisites:
- Go 1.27
- Docker (Colima or Docker Desktop)
- kind
- kubectl
- helm
- Terraform ≥ 1.5

None of the commands below use your ambient kubectl context. They all pin
`kind-steadmesh`.

```sh
make tools          # controller-gen, setup-envtest into ./bin
make test           # unit tests
make test-integration   # Postgres (testcontainers), envtest, Terraform acceptance
make e2e            # kind cluster, images, full apply-to-conversation suite with fakes
```

To deploy the example organisation yourself and message a representative,
follow [`docs/quickstart.html`](docs/quickstart.html).

To run with real Slack, Linear and Claude Code, see
[`docs/real-services.html`](docs/real-services.html) and
[`tests/live/README.md`](tests/live/README.md). The Slack app manifest is
[`docs/assets/slack-app-manifest.yaml`](docs/assets/slack-app-manifest.yaml).

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
