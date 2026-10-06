variable "name" {
  description = "ConfigMap name (DNS-1123 subdomain)."
  type        = string
  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$", var.name)) && length(var.name) <= 253
    error_message = "name must be a DNS-1123 subdomain."
  }
}

variable "namespace" {
  description = "Namespace of the ConfigMap: the organisation namespace, where the controller resolves configmap: references."
  type        = string
}

variable "source_path" {
  description = "Path of the markdown bundle file."
  type        = string
}

variable "key" {
  description = "ConfigMap data key. Defaults to the file's base name."
  type        = string
  default     = null
}

variable "labels" {
  description = "Extra labels for the ConfigMap."
  type        = map(string)
  default     = {}
}
