# Engineering specialisation of the base team template: development, review
# and coordination roles (design §4.2). Merge its team_templates with the
# base module's.

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
  bundles_dir = "${path.module}/../../bundles"
  role_files = {
    engineering_lead = "${local.bundles_dir}/roles/engineering_lead.md"
    engineer         = "${local.bundles_dir}/roles/engineer.md"
    reviewer         = "${local.bundles_dir}/roles/reviewer.md"
  }
}

module "team_bundle" {
  source      = "../instruction-bundle"
  name        = "${var.name_prefix}team-${replace(var.template_key, "_", "-")}"
  namespace   = var.namespace
  source_path = "${local.bundles_dir}/teams/engineering.md"
}

module "role_bundle" {
  source      = "../instruction-bundle"
  for_each    = local.role_files
  name        = "${var.name_prefix}role-${replace(each.key, "_", "-")}"
  namespace   = var.namespace
  source_path = each.value
}

locals {
  roles = merge({ for k, m in module.role_bundle : k => m.ref }, var.extra_roles)
  template = {
    extends          = var.extends
    instruction_refs = concat([module.team_bundle.ref], var.extra_instruction_refs)
    roles            = local.roles
    shared_memory    = var.shared_memory
    parameters       = var.parameters
  }
}
