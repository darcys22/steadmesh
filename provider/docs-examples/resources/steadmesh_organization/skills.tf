# Add local.skill_marketplaces to an organization's spec.skill_marketplaces.
# Add local.explicit_skills or local.agent_choice_skills to a seat's skills.
# Review a public marketplace's .claude-plugin/marketplace.json and skill files
# at the selected commit before applying. This example does not supply a pin.
variable "skill_marketplace_repository" {
  type        = string
  description = "Reviewed public GitHub marketplace in owner/repo form."
}

variable "skill_marketplace_commit" {
  type        = string
  description = "Reviewed full immutable commit SHA, never a branch or tag."
  validation {
    condition     = can(regex("^[a-f0-9]{40}$", var.skill_marketplace_commit))
    error_message = "Use a full lowercase 40-hex commit SHA."
  }
}

variable "skill_plugin" {
  type        = string
  description = "Plugin name from the pinned marketplace manifest."
}

variable "skill_name" {
  type        = string
  description = "Individual skill within that plugin, specified separately."
}

locals {
  skill_marketplaces = {
    reviewed = {
      repository = var.skill_marketplace_repository
      ref        = var.skill_marketplace_commit
    }
  }

  # Static Terraform selection: no catalog discovery or dynamic installation.
  explicit_skills = {
    mode         = "explicit"
    marketplaces = ["reviewed"]
    install = [{
      marketplace = "reviewed"
      plugin      = var.skill_plugin
      skill       = var.skill_name
    }]
  }

  # Opt in to selection by the agent, only from the approved pinned source.
  # Static installs remain enforced and do not consume the dynamic cap.
  agent_choice_skills = {
    mode               = "agent_choice"
    marketplaces       = ["reviewed"]
    install            = local.explicit_skills.install
    max_dynamic_skills = 16
  }

  # Discovery alone permits listing, not agent-selected installation.
  discovery_only_skills = {
    mode         = "discover"
    marketplaces = ["reviewed"]
  }
}
