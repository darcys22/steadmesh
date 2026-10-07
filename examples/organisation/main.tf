# Organisation stage (design §5.3): two human representatives and an
# engineering team (lead, engineer, reviewer), composed from the modules.

locals {
  bundles = "${path.module}/../../bundles"
}

# ---------------------------------------------------------------- instructions

module "culture" {
  source      = "../../modules/instruction-bundle"
  name        = "steadmesh-culture"
  namespace   = var.namespace
  source_path = "${local.bundles}/culture/culture.md"
}

module "representative_role" {
  source      = "../../modules/instruction-bundle"
  name        = "steadmesh-role-representative"
  namespace   = var.namespace
  source_path = "${local.bundles}/roles/representative.md"
}

module "team_base" {
  source    = "../../modules/team-base"
  namespace = var.namespace
}

module "team_engineering" {
  source    = "../../modules/team-engineering"
  namespace = var.namespace
  extends   = module.team_base.template_key
  shared_memory = {
    engineering = ["read", "search", "write", "revise", "archive", "history"]
  }
}

# ---------------------------------------------------------------- people

module "representative" {
  source           = "../../modules/representative"
  for_each         = var.humans
  human            = each.key
  display_name     = each.value.display_name
  external_user_id = each.value.slack_user_id
  connection       = "slack"
  role_ref         = module.representative_role.ref
}

locals {
  rep_seats = { for k, m in module.representative : k => m.seat_key }

  engineering_seats = {
    eng_lead = { role = "engineering_lead", display_name = "Engineering lead" }
    engineer = { role = "engineer", display_name = "Engineer" }
    reviewer = { role = "reviewer", display_name = "Reviewer" }
  }

  seats = merge(
    [for m in module.representative : m.seats]...
  )
  team_seats = {
    for k, s in local.engineering_seats : k => {
      role_ref          = "role:${s.role}"
      display_name      = s.display_name
      teams             = ["engineering"]
      harness_profile   = "primary"
      execution_profile = "interactive"
      sandbox_profile   = "standard"
      personal_memory   = k
      workspace         = { persistent = true }
    }
  }
  all_seat_keys = sort(concat(keys(local.seats), keys(local.team_seats)))

  is_claude = var.harness == "claude-code"
  model_connections = { for k, v in {
    model = {
      adapter      = "anthropic"
      endpoint_ref = var.model_endpoint_ref == "" ? null : var.model_endpoint_ref
      secret_ref   = var.secret_refs.anthropic
    }
  } : k => v if local.is_claude }
}

# ---------------------------------------------------------------- organisation

resource "steadmesh_organization" "this" {
  key            = var.organisation_key
  display_name   = var.display_name
  data_retention = "retain"
  wait_for_ready = var.wait_for_ready

  spec = {
    culture_refs   = [module.culture.ref]
    team_templates = merge(module.team_base.team_templates, module.team_engineering.team_templates)

    teams = {
      engineering = { template = module.team_engineering.template_key }
    }

    memory_stores = merge(
      {
        organisation = { retention = "retain" }
        engineering  = { retention = "retain" }
      },
      { for k in keys(local.team_seats) : k => { retention = "retain" } },
      [for m in module.representative : m.memory_stores]...
    )

    harness_profiles = {
      primary = {
        adapter          = var.harness
        image_digest     = var.harness_images[var.harness]
        model_connection = local.is_claude ? "model" : null
        model            = local.is_claude ? var.model : null
      }
    }

    execution_profiles = {
      interactive = {
        backend       = "kubernetes"
        service_class = "interactive"
        idle_policy   = "warm_then_stop"
        idle_timeout  = var.seat_idle_timeout
      }
    }

    sandbox_profiles = {
      standard = {}
    }

    connections = merge({
      slack = {
        adapter      = "slack"
        account_id   = var.slack_workspace_id
        endpoint_ref = var.slack_endpoint_ref == "" ? null : var.slack_endpoint_ref
        secret_ref   = var.secret_refs.slack
      }
      }, { for k, v in {
        linear = {
          adapter      = "linear"
          endpoint_ref = var.linear_endpoint_ref == "" ? null : var.linear_endpoint_ref
          secret_ref   = var.secret_refs.linear
          config       = { team_id = var.linear_team_id }
        }
    } : k => v if var.enable_linear }, local.model_connections)

    seats = merge(local.seats, local.team_seats)

    grants = merge(
      { for k, v in {
        engineering_linear = {
          subject    = "team:engineering"
          resource   = "connection:linear"
          operations = ["project.read", "project.create", "task.read", "task.write", "comment.read", "comment.write"]
        }
      } : k => v if var.enable_linear },
      # Representatives read the team's notes and work items, so they can
      # report progress without asking.
      { for h, seat in local.rep_seats : "engineering_read_${h}" => {
        subject    = "seat:${seat}"
        resource   = "memory:engineering"
        operations = ["read", "search"]
      } },
      { for k in local.all_seat_keys : "org_memory_${k}" => {
        subject    = "seat:${k}"
        resource   = "memory:organisation"
        operations = ["read", "search"]
      } },
    )

    message_routes = merge(
      { for h, seat in local.rep_seats : "${h}_to_eng_lead" => {
        from  = "seat:${seat}"
        to    = "seat:eng_lead"
        reply = true
      } },
      {
        eng_lead_engineer = { from = "seat:eng_lead", to = "seat:engineer", bidirectional = true }
        eng_lead_reviewer = { from = "seat:eng_lead", to = "seat:reviewer", bidirectional = true }
        engineer_reviewer = { from = "seat:engineer", to = "seat:reviewer", bidirectional = true }
      },
    )

    channel_bindings = merge([for m in module.representative : m.channel_bindings]...)

    # Optional: mirror engineering work items to the tracker for people to
    # follow. Agents coordinate in memory and messages either way.
    work_publication = var.enable_linear && var.publish_work_to_tracker ? { connection = "linear", stores = ["engineering"] } : null
  }

  timeouts {
    create = var.ready_timeout
    update = var.ready_timeout
    delete = var.ready_timeout
  }
}
