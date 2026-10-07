# Stage 2 of 2: declare your organisation. Apply after ../platform.
#
# Each person in `humans` gets a personal representative they message over
# Slack. Representatives delegate to a small engineering team (a lead and an
# engineer). Each seat runs a harness (Claude Code, Codex or Pi) on a model
# you choose. Edit instructions/*.md to set your culture and how
# representatives behave; add seats, teams and routes below as you grow.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    steadmesh = {
      source  = "darcys22/steadmesh"
      version = "~> 0.1.1"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
  }
}

# The platform stage's outputs: cluster, namespace, images and secret refs.
# If you keep state in a remote backend, point this at it instead.
data "terraform_remote_state" "platform" {
  backend = "local"
  config = {
    path = "${path.module}/../platform/terraform.tfstate"
  }
}

locals {
  platform  = data.terraform_remote_state.platform.outputs
  namespace = local.platform.organisation_namespace
  linear    = local.platform.secret_refs.linear != null
}

provider "steadmesh" {
  kubeconfig_path = local.platform.kubeconfig_path
  kube_context    = local.platform.kube_context
  namespace       = local.namespace
}

# Publishes the instruction bundles as ConfigMaps in the organisation namespace.
provider "kubernetes" {
  config_path    = local.platform.kubeconfig_path
  config_context = local.platform.kube_context
}

# ---------------------------------------------------------------- instructions

module "culture" {
  source      = "github.com/darcys22/steadmesh//modules/instruction-bundle?ref=v0.1.1"
  name        = "steadmesh-culture"
  namespace   = local.namespace
  source_path = "${path.module}/instructions/culture.md"
}

module "representative_role" {
  source      = "github.com/darcys22/steadmesh//modules/instruction-bundle?ref=v0.1.1"
  name        = "steadmesh-role-representative"
  namespace   = local.namespace
  source_path = "${path.module}/instructions/representative.md"
}

module "team_base" {
  source    = "github.com/darcys22/steadmesh//modules/team-base?ref=v0.1.1"
  namespace = local.namespace
}

module "team_engineering" {
  source    = "github.com/darcys22/steadmesh//modules/team-engineering?ref=v0.1.1"
  namespace = local.namespace
  extends   = module.team_base.template_key
}

# ---------------------------------------------------------------- people

module "representative" {
  source           = "github.com/darcys22/steadmesh//modules/representative?ref=v0.1.1"
  for_each         = var.humans
  human            = each.key
  display_name     = each.value.display_name
  external_user_id = each.value.slack_user_id
  connection       = "slack"
  role_ref         = module.representative_role.ref
  harness_profile  = contains(keys(var.seat_harnesses), "representative_${each.key}") ? "seat_representative_${each.key}" : "primary"
}

locals {
  engineering = {
    eng_lead = { role = "engineering_lead", display_name = "Engineering lead" }
    engineer = { role = "engineer", display_name = "Engineer" }
  }
  team_seats = {
    for k, s in local.engineering : k => {
      role_ref          = "role:${s.role}"
      display_name      = s.display_name
      teams             = ["engineering"]
      harness_profile   = contains(keys(var.seat_harnesses), k) ? "seat_${k}" : "primary"
      execution_profile = "interactive"
      sandbox_profile   = "standard"
      personal_memory   = k
      workspace         = { persistent = true }
    }
  }

  default_model = var.harness == "fake" ? null : var.model
  harness_profiles = merge(
    { primary = { adapter = var.harness, image_digest = local.platform.seat_images[var.harness], model = local.default_model } },
    { for k, h in var.seat_harnesses : "seat_${k}" => { adapter = h.adapter, image_digest = local.platform.seat_images[h.adapter], model = h.model } },
  )
  # Only the model connections some seat uses are declared; their keys come
  # from the platform stage.
  used_models = toset(compact(concat(
    [local.default_model == null ? "" : local.default_model.connection],
    [for h in var.seat_harnesses : h.model.connection],
  )))
  model_connections = { for k in local.used_models : k => merge(var.model_connections[k], { secret_ref = local.platform.secret_refs.models[k] }) }
}

# ---------------------------------------------------------------- organisation

resource "steadmesh_organization" "this" {
  key            = var.organisation_key
  display_name   = var.display_name
  data_retention = "retain"
  wait_for_ready = true

  spec = {
    culture_refs   = [module.culture.ref]
    team_templates = merge(module.team_base.team_templates, module.team_engineering.team_templates)
    teams          = { engineering = { template = module.team_engineering.template_key } }

    memory_stores = merge(
      { engineering = { retention = "retain" } },
      { for k in keys(local.team_seats) : k => { retention = "retain" } },
      [for m in module.representative : m.memory_stores]...
    )

    harness_profiles = local.harness_profiles

    execution_profiles = {
      interactive = {
        backend       = "kubernetes"
        service_class = "interactive"
        idle_policy   = "warm_then_stop"
        idle_timeout  = var.seat_idle_timeout
      }
    }

    sandbox_profiles = { standard = {} }

    connections = merge(
      {
        slack = {
          adapter      = "slack"
          account_id   = var.slack_workspace_id
          endpoint_ref = var.slack_endpoint_ref
          secret_ref   = local.platform.secret_refs.slack
        }
      },
      local.model_connections,
      { for k, v in {
        linear = {
          adapter      = "linear"
          endpoint_ref = var.linear_endpoint_ref
          secret_ref   = local.platform.secret_refs.linear
          config       = { team_id = var.linear_team_id }
        }
      } : k => v if local.linear },
    )

    seats = merge(local.team_seats, [for m in module.representative : m.seats]...)

    grants = merge(
      { for k, v in {
        engineering_linear = {
          subject    = "team:engineering"
          resource   = "connection:linear"
          operations = ["project.read", "project.create", "task.read", "task.write", "comment.read", "comment.write"]
        }
      } : k => v if local.linear },
      # Representatives read the team's notes and work items to report progress.
      { for h, m in module.representative : "engineering_read_${h}" => {
        subject    = "seat:${m.seat_key}"
        resource   = "memory:engineering"
        operations = ["read", "search"]
      } },
    )

    # Optional: mirror engineering work items to Linear for people to follow.
    work_publication = local.linear && var.publish_work_to_linear ? { connection = "linear", stores = ["engineering"] } : null

    message_routes = merge(
      { for h, m in module.representative : "${h}_to_eng_lead" => {
        from  = "seat:${m.seat_key}"
        to    = "seat:eng_lead"
        reply = true
      } },
      { eng_lead_engineer = { from = "seat:eng_lead", to = "seat:engineer", bidirectional = true } },
    )

    channel_bindings = merge([for m in module.representative : m.channel_bindings]...)
  }

  timeouts {
    create = "15m"
    update = "15m"
    delete = "15m"
  }
}
