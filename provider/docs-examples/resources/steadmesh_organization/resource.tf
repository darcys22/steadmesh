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
