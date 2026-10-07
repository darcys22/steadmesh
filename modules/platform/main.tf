# Installs Steadmesh into an existing cluster from a published release:
# namespaces, a Postgres for the platform's durable store, and the platform
# Helm chart (CRDs, controller, platform service and, optionally, the
# read-only console). Configure the kubernetes and helm providers in the
# calling root.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.30.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
  }
}

locals {
  tag = coalesce(var.image_tag, var.steadmesh_version)
  images = {
    controller      = "${var.image_registry}/controller"
    platform        = "${var.image_registry}/platform"
    console         = "${var.image_registry}/console"
    seat_fake       = "${var.image_registry}/seat-fake"
    seat_claudecode = "${var.image_registry}/seat-claudecode"
    seat_codex      = "${var.image_registry}/seat-codex"
    seat_pi         = "${var.image_registry}/seat-pi"
  }
}

resource "kubernetes_namespace_v1" "system" {
  metadata {
    name   = var.system_namespace
    labels = { "steadmesh.io/role" = "control-plane" }
  }
}

resource "kubernetes_namespace_v1" "organisation" {
  metadata {
    name   = var.organisation_namespace
    labels = { "steadmesh.io/role" = "organisation" }
  }
}

module "postgres" {
  source           = "../postgres"
  namespace        = kubernetes_namespace_v1.system.metadata[0].name
  postgres_image   = var.postgres_image
  postgres_storage = var.postgres_storage
}

resource "helm_release" "platform" {
  name       = "steadmesh"
  namespace  = kubernetes_namespace_v1.system.metadata[0].name
  repository = var.chart_path == null ? var.chart_repository : null
  chart      = coalesce(var.chart_path, "platform")
  version    = var.chart_path == null ? var.steadmesh_version : null
  # Wait for the Deployments so the organisation stage finds a serving API.
  wait    = true
  atomic  = false
  timeout = var.timeout_seconds

  values = [yamlencode(merge({
    image = {
      controllerRepository = local.images.controller
      platformRepository   = local.images.platform
      consoleRepository    = local.images.console
      tag                  = local.tag
      pullPolicy           = "IfNotPresent"
    }
    database = {
      secretName = module.postgres.database_secret
      secretKey  = "url"
    }
    controller = {
      # The NetworkPolicy enforcement probe runs the fake seat image.
      netprobeImage = "${local.images.seat_fake}:${local.tag}"
    }
    console = {
      enabled = var.enable_console
    }
  }, var.extra_values))]
}
