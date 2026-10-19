# Native team policy. Runtime pool members are not Terraform resource keys.
module "role" {
  source      = "../../modules/instruction-bundle"
  name        = "native-fixed-prompts-role"
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
  key            = "native-fixed-prompts"
  display_name   = "Self-organising peers with fixed prompts"
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
      board = { retention = "retain" }
      peer  = { retention = "retain" }
    }
    teams = {
      delivery = {
        coordination = {
          mode                       = "self_organising"
          mission                    = "Produce a reviewed plan for an offline sample project."
          work_store                 = "board"
          mission_editors            = ["team:delivery"]
          review_policy              = "required"
          max_messages_per_execution = 20
        }
        context = {
          editable_instructions = false
          editable_summary      = true
        }
        pool = {
          min_members           = 3
          max_members           = 12
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
      peer = merge(local.participant, { display_name = "Peer and human contact", teams = ["delivery"], personal_memory = "peer" })
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
        seat             = "peer"
        mode             = "direct_message"
      }
    }
  }
}
output "organisation_key" {
  value = steadmesh_organization.this.key
}
