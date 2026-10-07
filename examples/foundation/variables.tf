variable "kubeconfig_path" {
  description = "Kubeconfig containing the kind-steadmesh context (created by `kind create cluster`)."
  type        = string
  default     = "~/.kube/config"
}

variable "system_namespace" {
  description = "Control-plane namespace."
  type        = string
  default     = "steadmesh-system"
}

variable "organisation_namespace" {
  description = "Organisation namespace (one AgentOrganization per namespace)."
  type        = string
  default     = "steadmesh-example"
}

variable "postgres_image" {
  description = "Pinned Postgres image."
  type        = string
  default     = "postgres:16.4-alpine"
}

variable "postgres_storage" {
  description = "Postgres volume size."
  type        = string
  default     = "5Gi"
}

variable "enable_vault" {
  description = "Install Vault in dev mode (in-memory, root token; never for production). The default secret path is Kubernetes Secrets (secret_ref = \"k8s:<name>\")."
  type        = bool
  default     = false
}

variable "vault_chart_version" {
  description = "Pinned hashicorp/vault chart version."
  type        = string
  default     = "0.30.0"
}

variable "vault_dev_root_token" {
  description = "Vault dev-mode root token."
  type        = string
  default     = "steadmesh-dev-root"
  sensitive   = true
}

# Credentials. These become Kubernetes Secrets in the control-plane namespace,
# readable only by the platform ServiceAccount; the organisation root refers to
# them by name (secret_ref = "k8s:slack-credentials"), so their values never
# enter the organisation root's state or plans. They DO enter this foundation
# root's state (the kubernetes provider stores Secret data), so protect this
# state, or create the Secrets out of band and leave these at their defaults.
# The defaults are placeholders accepted by the in-repo fake Slack and Linear
# servers; set real values with TF_VAR_slack_bot_token etc. for live use.

variable "slack_bot_token" {
  description = "Slack bot token (xoxb-...)."
  type        = string
  default     = "xoxb-fake-bot-token"
  sensitive   = true
}

variable "slack_app_token" {
  description = "Slack app-level token for Socket Mode (xapp-...)."
  type        = string
  default     = "xapp-fake-app-token"
  sensitive   = true
}

variable "linear_api_key" {
  description = "Linear API key."
  type        = string
  default     = "lin_api_fake"
  sensitive   = true
}

variable "model_api_keys" {
  description = "API keys for model endpoints, keyed by model connection: each becomes the Secret <key>-credentials, referenced as k8s:<key>-credentials. The platform model proxy injects them; they never reach a seat."
  type        = map(string)
  default     = { anthropic = "sk-ant-fake" }
  sensitive   = true
}
