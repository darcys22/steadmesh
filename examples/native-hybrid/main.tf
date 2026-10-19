# Native team policy. Runtime pool members are not Terraform resource keys.
module "role" {
  source      = "../../modules/instruction-bundle"
  name        = "native-hybrid-role"
  namespace   = var.namespace
  source_path = "${path.module}/role.md"
}
locals {
  participant = {
    role_ref          = module.role.ref
    harness_profile   = "fake"
    execution_profile = "interactive"
    sandbox_profile   = "isolated"
    workspace         = { persistent = true }
  }
}
resource "steadmesh_organization" "this" {
  key            = "native-hybrid"
  display_name   = "Hybrid executive team"
  data_retention = "retain"
  wait_for_ready = var.wait_for_ready
  spec = {
    timezone = "Etc/UTC"
    model_budgets = {
      organization_daily_output_tokens = 100000
      execution_output_tokens          = 4000
    }
    harness_profiles = {
      fake = { adapter = "fake", image_digest = var.harness_image }
    }
    execution_profiles = {
      interactive = {
        backend       = "kubernetes"
        service_class = "interactive"
        idle_policy   = "warm_then_stop"
        idle_timeout  = "5m"
      }
    }
    sandbox_profiles = { isolated = {} }
    memory_stores = {
      board            = { retention = "retain" }
      ceo              = { retention = "retain" }
      engineering_lead = { retention = "retain" }
      operations_lead  = { retention = "retain" }
    }
    teams = {
      leadership = {
        coordination = { mode = "declared" }
      }
      delivery = {
        coordination = {
          mode                       = "self_organising"
          mission                    = "Produce a reviewed plan for an offline sample project."
          work_store                 = "board"
          mission_editors            = ["seat:ceo", "seat:engineering_lead", "seat:operations_lead"]
          review_policy              = "required"
          max_messages_per_execution = 20
        }
        context = {
          editable_instructions        = true
          require_instruction_approval = true
          instruction_approvers        = ["seat:ceo", "seat:engineering_lead", "seat:operations_lead"]
          editable_summary             = true
        }
        pool = {
          min_members           = 3
          max_members           = 30
          idle_timeout_seconds  = 900
          drain_timeout_seconds = 300
          retention_seconds     = 0 # Preserve retired private memory indefinitely.
          draining              = false
          templates = {
            contributor = {
              instructions = "Collaborate as a peer. Claim work on the board, preserve evidence, and request another member's review."
              seat         = local.participant
            }
          }
        }
      }
    }
    seats = {
      ceo              = merge(local.participant, { display_name = "CEO", teams = ["leadership", "delivery"], personal_memory = "ceo" })
      engineering_lead = merge(local.participant, { display_name = "Engineering lieutenant", teams = ["leadership", "delivery"], personal_memory = "engineering_lead" })
      operations_lead  = merge(local.participant, { display_name = "Operations lieutenant", teams = ["leadership", "delivery"], personal_memory = "operations_lead" })
    }
    grants = {
      delivery_board = {
        subject    = "team:delivery"
        resource   = "memory:board"
        operations = ["read", "search", "write", "revise", "archive", "history"]
      }
    }
    message_routes = {
      delivery_peers = {
        from          = "team:delivery"
        to            = "team:delivery"
        bidirectional = true
      }
    }
    # Reuse the local terminal fixture created by foundation with
    # -var='terminal_users=["operator"]'. No model or external-service secrets.
    connections = {
      terminal = { adapter = "terminal", secret_ref = "k8s:terminal-credentials" }
    }
    channel_bindings = {
      operator = {
        connection       = "terminal"
        external_user_id = "operator"
        seat             = "ceo"
        mode             = "direct_message"
      }
    }
  }
}
output "organisation_key" {
  value = steadmesh_organization.this.key
}
