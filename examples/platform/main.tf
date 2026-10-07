# Platform stage (design §5.3): installs the CRDs, controller and platform
# service from charts/platform and waits for them. The organisation stage
# runs only after this succeeds.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
  }
}

locals {
  kube_context = "kind-steadmesh" # pinned; the ambient context is never used

  # Keys consumed by charts/platform/values.yaml. extra_values is merged last
  # so a chart change can be accommodated without editing this root.
  values = merge({
    image = {
      tag        = var.image_tag
      pullPolicy = "IfNotPresent"
    }
    database = {
      secretName = var.database_secret
      secretKey  = "url"
    }
    vault = {
      enabled = var.vault_address != null
      address = var.vault_address
    }
    # Read-only Steadmesh Console; off unless enable_console is set.
    console = {
      enabled = var.enable_console
    }
    # Egress gateway for access profiles; off unless enable_egress is set.
    egress = {
      enabled = var.enable_egress
    }
  }, var.extra_values)
}

provider "helm" {
  kubernetes {
    config_path    = var.kubeconfig_path
    config_context = local.kube_context
  }
}

resource "helm_release" "platform" {
  name             = "steadmesh"
  chart            = "${path.module}/../../charts/platform"
  namespace        = var.system_namespace
  create_namespace = false
  # Wait for Deployments to be available, so the management API is served
  # before the organisation stage runs. A failed install is left in place for
  # diagnosis rather than rolled back silently.
  wait          = true
  wait_for_jobs = true
  atomic        = false
  timeout       = var.timeout_seconds
  values        = [yamlencode(local.values)]
}
