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

variable "existing_secret_refs" {
  description = <<-EOT
    Credentials you provision yourself, by reference (k8s:<secret name> in the
    control-plane namespace, or vault:<path>). A connection listed here gets no
    Terraform-managed Secret, so its value never enters Terraform state.
    Expected keys: slack (bot_token, app_token), linear (api_key), and in
    models one entry per model connection, e.g. anthropic or openai (api_key).
  EOT
  type = object({
    slack  = optional(string)
    linear = optional(string)
    models = optional(map(string), {})
  })
  default = {}
}

variable "slack_bot_token" {
  description = "Slack bot token (xoxb-...). Not needed with existing_secret_refs.slack."
  type        = string
  sensitive   = true
  default     = null
}

variable "slack_app_token" {
  description = "Slack app-level token for Socket Mode (xapp-...). Not needed with existing_secret_refs.slack."
  type        = string
  sensitive   = true
  default     = null
}

variable "model_api_keys" {
  description = "API keys for the model endpoints your agents use, keyed by model connection, e.g. { anthropic = \"sk-ant-...\", openai = \"sk-...\" }. Each becomes the Secret <key>-credentials. Not needed for keys in existing_secret_refs.models."
  type        = map(string)
  sensitive   = true
  default     = {}
}

variable "linear_api_key" {
  description = "Linear API key (lin_api_...). Optional: without it, agents have no tracker."
  type        = string
  sensitive   = true
  default     = null
}
