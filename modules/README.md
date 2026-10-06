# Terraform modules

Modules are usable straight from GitHub, pinned to a release, for example
`source = "github.com/darcys22/steadmesh//modules/platform?ref=v0.1.0"`;
`quickstart/` uses them that way.

## Installing the platform

| Module | What it does |
| --- | --- |
| `platform` | Installs a published release into an existing cluster: the control-plane and organisation namespaces, Postgres, and the platform chart from `oci://ghcr.io/darcys22/steadmesh/charts` with images from `ghcr.io/darcys22/steadmesh`. Outputs the namespaces, the seat image per harness, and how to open the console when `enable_console` is set. Configure the kubernetes and helm providers in the caller. |
| `postgres` | A single-instance Postgres plus the `steadmesh-db` Secret (key `url`) that the chart reads. Fine for trials and small installs; use a managed database in production. |

## Composing an organisation

Composition helpers for `steadmesh_organization`. Each module outputs plain HCL
objects that you merge into the resource's `spec`. Templates are resolved by
the provider, so a template change shows the affected seats in the plan
(`resolved_seats`).

| Module | Outputs |
| --- | --- |
| `instruction-bundle` | Creates a ConfigMap from a file in the organisation namespace (hashicorp/kubernetes provider). `ref = "configmap:<name>/<key>#sha256:<sha256 of content>"` |
| `team-base` | `team_templates` (`{ base = {...} }`), `template_key`, `roles` |
| `team-engineering` | `team_templates` (`{ engineering = { extends = "base", ... } }`), `roles` (`engineering_lead`, `engineer`, `reviewer`), `role_keys` |
| `team-accounting` | `team_templates` (`{ accounting = { extends = "base", ... } }`), `roles` (`accountant`) |
| `representative` | `seats`, `memory_stores` and `channel_bindings` entries for one human; also `seat_key` and `memory_store_key` |

Each team module publishes its own bundles from `bundles/`. Merge a base
module and a specialisation like this:

```hcl
team_templates = merge(module.team_base.team_templates, module.team_engineering.team_templates)
teams          = { engineering = { template = module.team_engineering.template_key } }
```

Seats on that team select a role with `role_ref = "role:reviewer"`.
`examples/organisation` shows a full composition.
