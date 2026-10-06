variable "namespace" {
  description = "Organisation namespace for the bundle ConfigMap."
  type        = string
}

variable "template_key" {
  description = "Key of the template in spec.team_templates."
  type        = string
  default     = "base"
}

variable "name_prefix" {
  description = "Prefix for bundle ConfigMap names."
  type        = string
  default     = "steadmesh-"
}

variable "bundle_path" {
  description = "Override the team guidance file (defaults to bundles/teams/base.md)."
  type        = string
  default     = null
}

variable "extra_instruction_refs" {
  description = "Additional instruction references appended after the base guidance."
  type        = list(string)
  default     = []
}

variable "roles" {
  description = "Role key to instruction reference available to every team derived from this template."
  type        = map(string)
  default     = {}
}

variable "shared_memory" {
  description = "Memory store key to operations granted to members of every derived team."
  type        = map(list(string))
  default     = {}
}

variable "parameters" {
  description = "Template parameters."
  type        = map(string)
  default     = {}
}
