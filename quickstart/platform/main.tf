# Stage 1 of 2: install Steadmesh into your cluster from the published
# release. Everything comes from GitHub and ghcr.io; no clone needed.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

provider "kubernetes" {
  config_path    = var.kubeconfig_path
  config_context = var.kube_context
}

provider "helm" {
  kubernetes {
    config_path    = var.kubeconfig_path
    config_context = var.kube_context
  }
}

module "steadmesh" {
  source = "github.com/darcys22/steadmesh//modules/platform?ref=v0.3.0"

  steadmesh_version      = "0.3.0"
  organisation_namespace = var.organisation_namespace
  enable_console         = var.enable_console
  enable_egress          = var.enable_egress
}

# Credentials, as Kubernetes Secrets in the control-plane namespace. Only the
# platform service can read them; the organisation stage refers to them by
# name.
#
# Secrets created here keep their values in this stage's Terraform state, so
# keep the state private. To keep credentials out of Terraform entirely,
# create the Secrets (or Vault entries) yourself and pass their references in
# existing_secret_refs: those are only referenced, never read here. Either
# way, rotating a value in place takes effect without running Terraform.

locals {
  create = {
    slack    = var.existing_secret_refs.slack == null && var.slack_bot_token != null
    terminal = var.existing_secret_refs.terminal == null && length(var.terminal_users) > 0
    linear   = var.existing_secret_refs.linear == null && var.linear_api_key != null
  }
  # Model connections whose key is given here rather than by reference.
  model_keys = toset([for k in nonsensitive(keys(var.model_api_keys)) : k if !contains(keys(var.existing_secret_refs.models), k)])
}

resource "terraform_data" "credentials" {
  lifecycle {
    precondition {
      condition     = (var.slack_bot_token == null) == (var.slack_app_token == null)
      error_message = "Set both slack_bot_token and slack_app_token, or neither."
    }
    precondition {
      condition     = length(local.model_keys) + length(var.existing_secret_refs.models) > 0
      error_message = "Set model_api_keys (e.g. { anthropic = \"sk-ant-...\" }), or existing_secret_refs.models."
    }
  }
}

resource "kubernetes_secret_v1" "slack" {
  count = local.create.slack ? 1 : 0
  metadata {
    name      = "slack-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    bot_token = var.slack_bot_token
    app_token = var.slack_app_token
  }
}

# Terminal chat (orgctl chat): one random bearer token per user, keyed by the
# user's terminal ID. orgctl reads it from this Secret.
resource "random_password" "terminal" {
  for_each = local.create.terminal ? toset(var.terminal_users) : toset([])
  length   = 40
  special  = false
}

resource "kubernetes_secret_v1" "terminal" {
  count = local.create.terminal ? 1 : 0
  metadata {
    name      = "terminal-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = { for u, p in random_password.terminal : u => p.result }
}

resource "kubernetes_secret_v1" "model" {
  for_each = local.model_keys
  metadata {
    name      = "${each.key}-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    api_key = var.model_api_keys[each.key]
  }
}

resource "kubernetes_secret_v1" "linear" {
  count = local.create.linear ? 1 : 0
  metadata {
    name      = "linear-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    api_key = var.linear_api_key
  }
}
