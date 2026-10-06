variable "kubeconfig_path" {
  type    = string
  default = "~/.kube/config"
}

variable "system_namespace" {
  description = "Control-plane namespace created by the foundation stage."
  type        = string
  default     = "steadmesh-system"
}

variable "image_tag" {
  description = "Tag of the steadmesh/* images loaded into kind (make kind-load TAG=...)."
  type        = string
  default     = "dev"
}

variable "database_secret" {
  description = "Secret in the control-plane namespace with key url (foundation output database_secret)."
  type        = string
  default     = "steadmesh-db"
}

variable "vault_address" {
  description = "Vault address when the foundation installed Vault (foundation output vault_address); null to use Kubernetes Secrets only."
  type        = string
  default     = null
}

variable "enable_console" {
  description = "Deploy the read-only Steadmesh Console and serve the platform's /console/v1 API to it. Off by default."
  type        = bool
  default     = false
}

variable "extra_values" {
  description = "Additional chart values, merged over the defaults above."
  type        = any
  default     = {}
}

variable "timeout_seconds" {
  type    = number
  default = 600
}
