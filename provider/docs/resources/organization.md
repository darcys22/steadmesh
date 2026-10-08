---
page_title: "steadmesh_organization Resource - Steadmesh"
subcategory: ""
description: |-
    An organisation of persistent agents (one AgentOrganization object). The provider owns its declared fields; the controller owns seats and status.
---

# steadmesh_organization (Resource)

An organisation of persistent agents (one AgentOrganization object). The provider owns its declared fields; the controller owns seats and status.

The provider validates and compiles the declaration during `plan`, as soon as its values are known: team templates are merged, `role:<name>` references are resolved through each seat's team templates, and grants and routes are checked against the seats and connections they name. Mistakes therefore fail the plan, not the apply. `resolved_seats` shows each seat's effective role and configuration revision, so a change to a shared template or instruction file shows which seats it affects.

## Example Usage

```terraform
# Instruction text is published as ConfigMaps in the organisation namespace and
# referenced by content digest, so editing a file shows up as a plan change.
module "culture" {
  source      = "github.com/darcys22/steadmesh//modules/instruction-bundle?ref=v0.4.1"
  name        = "acme-culture"
  namespace   = "acme-org"
  source_path = "${path.module}/instructions/culture.md"
}

module "representative_role" {
  source      = "github.com/darcys22/steadmesh//modules/instruction-bundle?ref=v0.4.1"
  name        = "acme-role-representative"
  namespace   = "acme-org"
  source_path = "${path.module}/instructions/representative.md"
}

module "engineer_role" {
  source      = "github.com/darcys22/steadmesh//modules/instruction-bundle?ref=v0.4.1"
  name        = "acme-role-engineer"
  namespace   = "acme-org"
  source_path = "${path.module}/instructions/engineer.md"
}

# Alice messages her representative over Slack. The representative delegates
# to an engineer, who can create Linear issues.
resource "steadmesh_organization" "acme" {
  key          = "acme"
  display_name = "Acme"

  spec = {
    culture_refs = [module.culture.ref]

    team_templates = {
      engineering = { roles = { engineer = module.engineer_role.ref } }
    }
    teams = {
      engineering = { template = "engineering" }
    }

    harness_profiles = {
      claude = {
        adapter      = "claude-code"
        image_digest = "ghcr.io/darcys22/steadmesh/seat-claudecode:0.4.1"
        model        = { connection = "anthropic", id = "claude-sonnet-5-5" }
      }
      # Another seat could run Codex on OpenAI, or Pi on any compatible endpoint:
      # codex = { adapter = "codex", image_digest = "…/seat-codex:0.1.1",
      #           model = { connection = "openai", id = "gpt-5.5" } }
    }
    execution_profiles = {
      interactive = {
        backend      = "kubernetes"
        idle_policy  = "warm_then_stop"
        idle_timeout = "30m"
      }
    }
    sandbox_profiles = { standard = {} }

    memory_stores = {
      rep_alice = { retention = "retain" }
      engineer  = { retention = "retain" }
    }

    seats = {
      representative_alice = {
        display_name      = "Alice's representative"
        role_ref          = module.representative_role.ref
        harness_profile   = "claude"
        execution_profile = "interactive"
        sandbox_profile   = "standard"
        personal_memory   = "rep_alice"
        workspace         = { persistent = true }
      }
      engineer = {
        display_name      = "Engineer"
        role_ref          = "role:engineer" # resolved through the team template
        teams             = ["engineering"]
        harness_profile   = "claude"
        execution_profile = "interactive"
        sandbox_profile   = "standard"
        personal_memory   = "engineer"
        workspace         = { persistent = true }
      }
    }

    # Secrets are referenced, never inlined: k8s:<name> is a Kubernetes Secret
    # in the control-plane namespace, vault:<path> a Vault KV entry. Slack
    # needs bot_token and app_token; Anthropic and Linear need api_key.
    connections = {
      slack     = { adapter = "slack", account_id = "T0123456", secret_ref = "k8s:slack-credentials" }
      anthropic = { adapter = "anthropic", secret_ref = "k8s:anthropic-credentials" }
      linear    = { adapter = "linear", secret_ref = "k8s:linear-credentials", config = { team_id = "ENG" } }
    }

    grants = {
      engineering_linear = {
        subject    = "team:engineering"
        resource   = "connection:linear"
        operations = ["task.write"]
      }
    }

    message_routes = {
      alice_to_engineer = { from = "seat:representative_alice", to = "seat:engineer", reply = true }
    }

    channel_bindings = {
      alice = {
        connection       = "slack"
        external_user_id = "U0ALICE"
        seat             = "representative_alice"
        mode             = "direct_message"
      }
    }
  }

  timeouts {
    create = "15m"
    update = "15m"
  }
}

output "representatives" {
  value = steadmesh_organization.acme.connection_details.representatives
}
```

Instruction text (culture, team and role guidance) lives in ConfigMaps referenced as `configmap:<name>/<key>#sha256:<digest>`. The [`instruction-bundle`](https://github.com/darcys22/steadmesh/tree/main/modules/instruction-bundle) module publishes a Markdown file and returns that reference. The [`representative`](https://github.com/darcys22/steadmesh/tree/main/modules/representative), [`team-base`](https://github.com/darcys22/steadmesh/tree/main/modules/team-base) and [`team-engineering`](https://github.com/darcys22/steadmesh/tree/main/modules/team-engineering) modules produce ready-made entries for `spec`.

## Readiness

With `wait_for_ready = true` (the default), create and update return only once the controller reports `OperationalReady=True` for the new generation. That means seats are provisioned, connections authenticated, channel ingress running and routes executable. If readiness is not reached within the timeout, the apply fails and lists the conditions that are not ready; the same conditions are in the `conditions` attribute.

A failed create leaves the resource tainted, so the next plan replaces it. After fixing the reported condition, untaint it rather than recreating the organisation: `terraform untaint steadmesh_organization.<name>`.

## Data retention

`data_retention` controls what happens when the resource is destroyed. With `retain` (the default), the agents are stopped and their access revoked, but workspace volumes and platform records (conversations, memory) are kept; a seat declared later with `adopt_from` can take over a retired seat's data. With `delete`, the volumes and records are deleted too. External resources such as Slack workspaces and Linear projects are never deleted.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `display_name` (String) Human-readable organisation name.
- `key` (String) Stable organisation key; also the AgentOrganization object name. Changing it replaces the organisation.
- `spec` (Attributes) Typed organisation specification. Templates are resolved by the provider; the object receives the resolved specification. (see [below for nested schema](#nestedatt--spec))

### Optional

- `data_retention` (String) Data retention on organisation deletion: retain (the default applied by the compiler) or delete.
- `timeouts` (Block, Optional) (see [below for nested schema](#nestedblock--timeouts))
- `wait_for_ready` (Boolean) Wait for OperationalReady=True on the current generation after create and update.

### Read-Only

- `conditions` (List of Object) Observed status conditions, sorted by type. Each has type (e.g. OperationalReady, ConnectionsAuthenticated), status (True, False or Unknown), reason and message. (see [below for nested schema](#nestedatt--conditions))
- `connection_details` (Attributes) Stable, non-secret connection details reported by the controller. (see [below for nested schema](#nestedatt--connection_details))
- `effective_revision` (String) Effective revision reported by the controller.
- `id` (String) Immutable organisation ID assigned by the platform (status.organizationID); the object UID until the controller has assigned one.
- `manifest_digest` (String) Digest of the compiled effective manifest. Changes when the declaration, a template or the live object changes.
- `resolved_seats` (Map of Object) Compiled per-seat summary, so template changes show the affected seats in the plan. Keyed by seat key; each entry has role_ref, teams, harness_profile and config_revision (the seat's configuration revision). (see [below for nested schema](#nestedatt--resolved_seats))

<a id="nestedatt--spec"></a>
### Nested Schema for `spec`

Optional:

- `access_profiles` (Attributes Map) Sandbox access profiles: practical access from a seat's sandbox, one plugin per field. Seats and teams name them; a seat gets the union. Without any, a seat reaches only the platform. See docs/sandbox.html. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--access_profiles))
- `channel_bindings` (Attributes Map) Bindings of verified human identities to representative seats. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--channel_bindings))
- `connections` (Attributes Map) Connections to external services. Secrets are references only. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--connections))
- `culture_refs` (List of String) Ordered organisation culture instruction references (configmap:<name>/<key>#sha256:<digest>).
- `execution_profiles` (Attributes Map) Execution profiles. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--execution_profiles))
- `grants` (Attributes Map) Access grants. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--grants))
- `harness_profiles` (Attributes Map) Harness profiles. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--harness_profiles))
- `memory_stores` (Attributes Map) Memory stores. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--memory_stores))
- `message_routes` (Attributes Map) Permitted message routes. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--message_routes))
- `sandbox_profiles` (Attributes Map) Sandbox profiles. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--sandbox_profiles))
- `seats` (Attributes Map) Seats: persistent agent identities. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--seats))
- `shared_workspaces` (Attributes Map) Shared workspaces. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--shared_workspaces))
- `team_templates` (Attributes Map) Reusable team templates. Resolved by the provider before the object is written. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--team_templates))
- `teams` (Attributes Map) Concrete teams. Membership is declared on seats. Keyed by stable key. (see [below for nested schema](#nestedatt--spec--teams))
- `work_publication` (Attributes) Publish the work items of shared memory stores to a tracker so people can follow progress there. Optional: agents coordinate through memory and messages either way, and a failing tracker never blocks them. Personal stores are never published. (see [below for nested schema](#nestedatt--spec--work_publication))

<a id="nestedatt--spec--access_profiles"></a>
### Nested Schema for `spec.access_profiles`

Optional:

- `browser` (Attributes) A headless browser tool (needs a -browser seat image); its traffic goes through the egress gateway. (see [below for nested schema](#nestedatt--spec--access_profiles--browser))
- `egress` (Attributes) Hosts the seat may reach through the egress gateway (enable_egress). Changes apply live; removed hosts close open connections within seconds. (see [below for nested schema](#nestedatt--spec--access_profiles--egress))
- `github` (Attributes) Repository access through a github connection. (see [below for nested schema](#nestedatt--spec--access_profiles--github))
- `network` (Attributes) Direct connections to IP ranges, enforced by NetworkPolicy. (see [below for nested schema](#nestedatt--spec--access_profiles--network))
- `tools` (Attributes) Binaries the seat image must provide; the seat does not start without them. (see [below for nested schema](#nestedatt--spec--access_profiles--tools))

<a id="nestedatt--spec--access_profiles--browser"></a>
### Nested Schema for `spec.access_profiles.browser`

Optional:

- `session` (Attributes) A signed-in session to load. (see [below for nested schema](#nestedatt--spec--access_profiles--browser--session))

<a id="nestedatt--spec--access_profiles--browser--session"></a>
### Nested Schema for `spec.access_profiles.browser.session`

Required:

- `connection` (String) A browser_session connection.



<a id="nestedatt--spec--access_profiles--egress"></a>
### Nested Schema for `spec.access_profiles.egress`

Required:

- `hosts` (List of String) example.com, *.example.com (subdomains) or host:port. Without a port, 443 and 80.


<a id="nestedatt--spec--access_profiles--github"></a>
### Nested Schema for `spec.access_profiles.github`

Required:

- `connection` (String) A github connection.
- `permissions` (Map of String) contents, pull_requests, issues or metadata => read or write.
- `repos` (List of String) owner/name repositories.

Optional:

- `delivery` (String) platform (default): operations through the platform; the credential never enters the sandbox. sandbox: git and gh in the sandbox receive a credential (a scoped, hour-long token with a GitHub App).


<a id="nestedatt--spec--access_profiles--network"></a>
### Nested Schema for `spec.access_profiles.network`

Required:

- `rules` (Attributes List) Allowed ranges. (see [below for nested schema](#nestedatt--spec--access_profiles--network--rules))

<a id="nestedatt--spec--access_profiles--network--rules"></a>
### Nested Schema for `spec.access_profiles.network.rules`

Required:

- `cidr` (String) IP range, e.g. 10.0.5.0/24.

Optional:

- `ports` (List of Number) Ports; empty allows every port.
- `protocol` (String) tcp (default), udp or sctp.



<a id="nestedatt--spec--access_profiles--tools"></a>
### Nested Schema for `spec.access_profiles.tools`

Required:

- `binaries` (List of String) Binary names, e.g. git, gh, curl.



<a id="nestedatt--spec--channel_bindings"></a>
### Nested Schema for `spec.channel_bindings`

Required:

- `connection` (String) Communication connection key.
- `external_user_id` (String) Verified external user ID.
- `seat` (String) Representative seat key.

Optional:

- `mode` (String) direct_message.


<a id="nestedatt--spec--connections"></a>
### Nested Schema for `spec.connections`

Required:

- `adapter` (String) Connector adapter: slack, terminal, linear, anthropic, openai or model (any compatible model endpoint).

Optional:

- `account_id` (String) Authorised account or workspace identity.
- `config` (Map of String) Adapter configuration, e.g. team_id for linear.
- `endpoint_ref` (String) Base URL override (fakes, self-hosted). For model connections, the API base, usually ending in /v1.
- `model` (Attributes) What a model connection serves. Defaults for anthropic and openai; the model adapter must declare its APIs. Claims are verified at readiness. (see [below for nested schema](#nestedatt--spec--connections--model))
- `ownership` (String) external (default) or managed.
- `required` (Boolean) Whether the connection must authenticate before the organisation is ready. By default model connections a harness uses and communication connections with channel bindings are required; others, such as a work tracker, are optional: their failures are reported as IntegrationsDegraded but never block readiness or internal work.
- `secret_ref` (String) vault:<path> or k8s:<secret-name>. Never a raw credential.

<a id="nestedatt--spec--connections--model"></a>
### Nested Schema for `spec.connections.model`

Optional:

- `apis` (List of String) APIs the endpoint serves: anthropic_messages, openai_responses, openai_chat.
- `auth` (String) How the credential is sent: bearer (default), x-api-key or header:<Name>.
- `models` (Attributes List) Models the endpoint serves. When set, harness profiles may only select these, and readiness checks each. (see [below for nested schema](#nestedatt--spec--connections--model--models))
- `verify` (String) Readiness check: request (default; a minimal request per API and model), models (list models) or none.

<a id="nestedatt--spec--connections--model--models"></a>
### Nested Schema for `spec.connections.model.models`

Required:

- `id` (String) Model identifier.

Optional:

- `apis` (List of String) APIs this model is served on; empty means all of the connection's APIs.




<a id="nestedatt--spec--execution_profiles"></a>
### Nested Schema for `spec.execution_profiles`

Required:

- `backend` (String) Sandbox backend (kubernetes).

Optional:

- `cpu_limit` (String) CPU limit quantity.
- `cpu_request` (String) CPU request quantity.
- `idle_policy` (String) warm, warm_then_stop or suspend.
- `idle_timeout` (String) Idle timeout duration, at least 1m.
- `memory_limit` (String) Memory limit quantity.
- `memory_request` (String) Memory request quantity.
- `required_features` (List of String) Backend features that must be supported.
- `service_class` (String) interactive or background.
- `storage_class` (String) Storage class of per-seat workspace volumes (cluster default when empty).
- `workspace_size_gb` (Number) Per-seat workspace size in GB.


<a id="nestedatt--spec--grants"></a>
### Nested Schema for `spec.grants`

Required:

- `operations` (List of String) Allowed operations.
- `resource` (String) connection:<key>, memory:<key> or workspace:<key>.
- `subject` (String) seat:<key> or team:<key>.

Optional:

- `targets` (List of String) Optional target restrictions.


<a id="nestedatt--spec--harness_profiles"></a>
### Nested Schema for `spec.harness_profiles`

Required:

- `adapter` (String) Harness adapter: claude-code, codex, pi or fake.
- `image_digest` (String) Pinned harness image.

Optional:

- `config` (Map of String) Adapter configuration.
- `model` (Attributes) The model the harness uses and the connection that serves it. (see [below for nested schema](#nestedatt--spec--harness_profiles--model))
- `required_capabilities` (List of String) Capabilities the adapter must support.

<a id="nestedatt--spec--harness_profiles--model"></a>
### Nested Schema for `spec.harness_profiles.model`

Required:

- `connection` (String) A model connection.
- `id` (String) Model identifier sent to the endpoint. The platform rejects requests from the seat for any other model.

Optional:

- `api` (String) anthropic_messages, openai_responses or openai_chat. When unset, the first API the harness speaks that the connection and model also serve.
- `settings` (Map of String) Harness-specific model settings: effort (claude-code); reasoning_effort (codex); thinking, context_window, max_tokens, reasoning (pi).



<a id="nestedatt--spec--memory_stores"></a>
### Nested Schema for `spec.memory_stores`

Optional:

- `backing_class` (String) Backing store class (postgres).
- `capacity_mb` (Number) Declared capacity in MB.
- `retention` (String) retain (default) or delete.


<a id="nestedatt--spec--message_routes"></a>
### Nested Schema for `spec.message_routes`

Required:

- `from` (String) seat:<key> or team:<key>.
- `to` (String) seat:<key> or team:<key>.

Optional:

- `bidirectional` (Boolean) Permit both directions.
- `reply` (Boolean) Recipient may reply within conversations opened on this route.


<a id="nestedatt--spec--sandbox_profiles"></a>
### Nested Schema for `spec.sandbox_profiles`

Optional:

- `filesystem_policy_ref` (String) Filesystem policy (workspace_only).
- `network_policy_ref` (String) Network policy (deny_all_except_platform).
- `required_enforcement` (List of String) Enforcement features that must be active.
- `runtime_class` (String) Kubernetes RuntimeClass.


<a id="nestedatt--spec--seats"></a>
### Nested Schema for `spec.seats`

Required:

- `execution_profile` (String) Execution profile key.
- `harness_profile` (String) Harness profile key.
- `role_ref` (String) Role instruction reference, or role:<key> resolved from the seat's team templates.
- `sandbox_profile` (String) Sandbox profile key.

Optional:

- `access_profiles` (List of String) Access profiles granted to this seat, in addition to its teams'.
- `adopt_from` (String) Explicitly adopt the retained data of a retired seat ID.
- `display_name` (String) Display name.
- `instruction_refs` (List of String) Additional seat-scoped instruction references.
- `personal_memory` (String) Personal memory store key.
- `teams` (List of String) Ordered team memberships.
- `workspace` (Attributes) Workspace binding. (see [below for nested schema](#nestedatt--spec--seats--workspace))

<a id="nestedatt--spec--seats--workspace"></a>
### Nested Schema for `spec.seats.workspace`

Optional:

- `persistent` (Boolean) Persistent per-seat workspace.
- `shared` (List of String) Shared workspace keys to mount (requires a grant).



<a id="nestedatt--spec--shared_workspaces"></a>
### Nested Schema for `spec.shared_workspaces`

Required:

- `access_mode` (String) ReadWriteMany or ReadOnlyMany.

Optional:

- `retention` (String) retain (default) or delete.
- `size_gb` (Number) Size in GB.
- `storage_class` (String) Storage class.


<a id="nestedatt--spec--team_templates"></a>
### Nested Schema for `spec.team_templates`

Optional:

- `extends` (String) Template this template extends.
- `instruction_refs` (List of String) Ordered team instruction references.
- `parameters` (Map of String) Template parameters.
- `roles` (Map of String) Role key to role instruction reference; seats select one with role_ref = "role:<key>".
- `shared_memory` (Map of List of String) Memory store key to operations granted to members of a team using this template.


<a id="nestedatt--spec--teams"></a>
### Nested Schema for `spec.teams`

Optional:

- `access_profiles` (List of String) Access profiles granted to every member.
- `instruction_refs` (List of String) Additional ordered team instruction references.
- `parameters` (Map of String) Parameters overriding template parameters.
- `shared_memory` (Map of List of String) Memory store key to operations granted to members.
- `template` (String) Team template to instantiate.


<a id="nestedatt--spec--work_publication"></a>
### Nested Schema for `spec.work_publication`

Required:

- `connection` (String) Tracker connection key, e.g. a Linear connection.
- `stores` (List of String) Shared memory stores whose work items are published.



<a id="nestedblock--timeouts"></a>
### Nested Schema for `timeouts`

Optional:

- `create` (String) A string that can be [parsed as a duration](https://pkg.go.dev/time#ParseDuration) consisting of numbers and unit suffixes, such as "30s" or "2h45m". Valid time units are "s" (seconds), "m" (minutes), "h" (hours).
- `delete` (String) A string that can be [parsed as a duration](https://pkg.go.dev/time#ParseDuration) consisting of numbers and unit suffixes, such as "30s" or "2h45m". Valid time units are "s" (seconds), "m" (minutes), "h" (hours). Setting a timeout for a Delete operation is only applicable if changes are saved into state before the destroy operation occurs.
- `update` (String) A string that can be [parsed as a duration](https://pkg.go.dev/time#ParseDuration) consisting of numbers and unit suffixes, such as "30s" or "2h45m". Valid time units are "s" (seconds), "m" (minutes), "h" (hours).


<a id="nestedatt--conditions"></a>
### Nested Schema for `conditions`

Read-Only:

- `message` (String)
- `reason` (String)
- `status` (String)
- `type` (String)


<a id="nestedatt--connection_details"></a>
### Nested Schema for `connection_details`

Read-Only:

- `namespace` (String) Namespace the organisation's seats run in.
- `organization_id` (String) Immutable organisation ID assigned by the platform.
- `representatives` (Map of Object) Representative seats by human, keyed by channel binding. Each has seat, seat_id, connection, adapter, external_user_id and mode, so you can tell people which identity reaches their representative. (see [below for nested schema](#nestedatt--connection_details--representatives))
- `status_endpoint` (String) Platform URL of the organisation's runtime status. It is on the internal API, which accepts only the controller's identity.

<a id="nestedatt--connection_details--representatives"></a>
### Nested Schema for `connection_details.representatives`

Read-Only:

- `adapter` (String)
- `connection` (String)
- `external_user_id` (String)
- `mode` (String)
- `seat` (String)
- `seat_id` (String)



<a id="nestedatt--resolved_seats"></a>
### Nested Schema for `resolved_seats`

Read-Only:

- `config_revision` (String)
- `harness_profile` (String)
- `role_ref` (String)
- `teams` (List of String)

## Import

```shell
# Import an organisation into this configuration by namespace/key.
terraform import steadmesh_organization.acme acme-org/acme

# Adopt an AgentOrganization that has no declaration owner yet.
terraform import steadmesh_organization.acme acme-org/acme/adopt
```

An organisation already owned by another declaration owner, for example a Helm release, cannot be imported.
