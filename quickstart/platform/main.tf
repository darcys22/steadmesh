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
  source = "github.com/darcys22/steadmesh//modules/platform?ref=v0.1.1"

  steadmesh_version      = "0.1.1"
  organisation_namespace = var.organisation_namespace
  enable_console         = var.enable_console
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
    slack     = var.existing_secret_refs.slack == null
    anthropic = var.existing_secret_refs.anthropic == null
    linear    = var.existing_secret_refs.linear == null && var.linear_api_key != null
  }
}

resource "terraform_data" "credentials" {
  lifecycle {
    precondition {
      condition     = !local.create.slack || (var.slack_bot_token != null && var.slack_app_token != null)
      error_message = "Set slack_bot_token and slack_app_token, or existing_secret_refs.slack."
    }
    precondition {
      condition     = !local.create.anthropic || var.anthropic_api_key != null
      error_message = "Set anthropic_api_key, or existing_secret_refs.anthropic."
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

resource "kubernetes_secret_v1" "anthropic" {
  count = local.create.anthropic ? 1 : 0
  metadata {
    name      = "anthropic-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    api_key = var.anthropic_api_key
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
