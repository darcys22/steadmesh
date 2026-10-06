# General team template (design §4.2). Specialisations such as
# team-engineering and team-accounting extend it by key.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.30.0"
    }
  }
}

module "bundle" {
  source      = "../instruction-bundle"
  name        = "${var.name_prefix}team-${replace(var.template_key, "_", "-")}"
  namespace   = var.namespace
  source_path = coalesce(var.bundle_path, "${path.module}/../../bundles/teams/base.md")
}

locals {
  template = {
    instruction_refs = concat([module.bundle.ref], var.extra_instruction_refs)
    roles            = var.roles
    shared_memory    = var.shared_memory
    parameters       = var.parameters
  }
}
