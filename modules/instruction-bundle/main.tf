# Publishes one instruction bundle as a ConfigMap and returns an immutable,
# digest-pinned reference for spec culture_refs / instruction_refs / role_ref.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.30.0"
    }
  }
}

locals {
  content = file(var.source_path)
  key     = coalesce(var.key, basename(var.source_path))
  digest  = sha256(local.content)
}

resource "kubernetes_config_map_v1" "this" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels = merge(var.labels, {
      "app.kubernetes.io/managed-by" = "terraform"
      "steadmesh.io/component"       = "instruction-bundle"
    })
    annotations = {
      "steadmesh.io/bundle-sha256" = local.digest
    }
  }
  data = {
    (local.key) = local.content
  }
}
