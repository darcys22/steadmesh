variable "namespace" {
  description = "Organisation namespace for the bundle ConfigMaps."
  type        = string
}

variable "extends" {
  description = "Template key this template extends (module.team_base.template_key)."
  type        = string
  default     = "base"
}

variable "template_key" {
  description = "Key of the template in spec.team_templates."
  type        = string
  default     = "engineering"
}

variable "name_prefix" {
  description = "Prefix for bundle ConfigMap names."
  type        = string
  default     = "steadmesh-"
}

variable "shared_memory" {
  description = "Memory store key to operations granted to members (the stores must exist in spec.memory_stores)."
  type        = map(list(string))
  default = {
    engineering = ["read", "search", "write", "revise", "archive", "publish", "history"]
  }
}

variable "extra_roles" {
  description = "Additional or overriding role references."
  type        = map(string)
  default     = {}
}

variable "extra_instruction_refs" {
  description = "Additional team instruction references."
  type        = list(string)
  default     = []
}

variable "parameters" {
  description = "Template parameters."
  type        = map(string)
  default     = {}
}
