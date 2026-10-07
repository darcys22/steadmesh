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
# name. Their values are stored in this stage's Terraform state, so keep the
# state private, or create the Secrets yourself and drop these resources.

resource "kubernetes_secret_v1" "slack" {
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
  metadata {
    name      = "anthropic-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    api_key = var.anthropic_api_key
  }
}

resource "kubernetes_secret_v1" "linear" {
  count = var.linear_api_key == null ? 0 : 1
  metadata {
    name      = "linear-credentials"
    namespace = module.steadmesh.system_namespace
  }
  data = {
    api_key = var.linear_api_key
  }
}
