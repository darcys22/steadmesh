variable "steadmesh_version" {
  description = "Release to install, without the leading v (e.g. 0.1.0). Selects the chart version and, unless image_tag is set, the image tag."
  type        = string
}

variable "system_namespace" {
  description = "Control-plane namespace (created)."
  type        = string
  default     = "steadmesh-system"
}

variable "organisation_namespace" {
  description = "Namespace the organisation's seats run in (created)."
  type        = string
  default     = "steadmesh"
}

variable "enable_console" {
  description = "Deploy the read-only Steadmesh Console. Off by default."
  type        = bool
  default     = false
}

variable "image_registry" {
  description = "Registry and path prefix of the Steadmesh images."
  type        = string
  default     = "ghcr.io/darcys22/steadmesh"
}

variable "image_tag" {
  description = "Image tag override; defaults to steadmesh_version."
  type        = string
  default     = null
}

variable "chart_repository" {
  description = "Helm repository of the platform chart."
  type        = string
  default     = "oci://ghcr.io/darcys22/steadmesh/charts"
}

variable "chart_path" {
  description = "Local chart directory to install instead of the published chart (development only)."
  type        = string
  default     = null
}

variable "postgres_image" {
  type    = string
  default = "postgres:16.4-alpine"
}

variable "postgres_storage" {
  type    = string
  default = "5Gi"
}

variable "timeout_seconds" {
  description = "How long to wait for the platform to become available."
  type        = number
  default     = 600
}

variable "extra_values" {
  description = "Additional chart values, merged over the module's (top-level keys replace)."
  type        = any
  default     = {}
}
