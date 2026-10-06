variable "kube_context" {
  description = "kubeconfig context of the cluster to install into. Required: the current context is never assumed."
  type        = string
}

variable "kubeconfig_path" {
  type    = string
  default = "~/.kube/config"
}

variable "organisation_namespace" {
  description = "Namespace your organisation's agents run in."
  type        = string
  default     = "steadmesh"
}

variable "enable_console" {
  description = "Deploy the read-only Steadmesh Console."
  type        = bool
  default     = false
}

variable "slack_bot_token" {
  description = "Slack bot token (xoxb-...)."
  type        = string
  sensitive   = true
}

variable "slack_app_token" {
  description = "Slack app-level token for Socket Mode (xapp-...)."
  type        = string
  sensitive   = true
}

variable "anthropic_api_key" {
  description = "Anthropic API key the agents' Claude Code harness uses."
  type        = string
  sensitive   = true
}

variable "linear_api_key" {
  description = "Linear API key (lin_api_...). Optional: without it, agents have no tracker."
  type        = string
  sensitive   = true
  default     = null
}
