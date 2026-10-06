variable "kubeconfig_path" {
  type    = string
  default = "~/.kube/config"
}

variable "namespace" {
  description = "Organisation namespace created by the foundation stage."
  type        = string
  default     = "steadmesh-example"
}

variable "organisation_key" {
  type    = string
  default = "example-company"
}

variable "display_name" {
  type    = string
  default = "Example Company"
}

variable "humans" {
  description = "People with a personal representative: key => Slack user ID and display name. The defaults match the in-repo fake Slack server."
  type = map(object({
    slack_user_id = string
    display_name  = optional(string)
  }))
  default = {
    sean = { slack_user_id = "U0SEAN", display_name = "Sean's representative" }
    alex = { slack_user_id = "U0ALEX", display_name = "Alex's representative" }
  }
  validation {
    condition     = length(var.humans) >= 2
    error_message = "The acceptance organisation has at least two human representatives."
  }
}

variable "harness" {
  description = "Harness for every seat: claude-code (real model) or fake (deterministic)."
  type        = string
  default     = "fake"
  validation {
    condition     = contains(["claude-code", "fake"], var.harness)
    error_message = "harness must be \"claude-code\" or \"fake\"."
  }
}

variable "harness_images" {
  description = "Pinned seat images per harness (image@sha256:... in production; local tags in kind)."
  type        = map(string)
  default = {
    "claude-code" = "steadmesh/seat-claudecode:dev"
    "fake"        = "steadmesh/seat-fake:dev"
  }
}

variable "model" {
  description = "Model name for the claude-code harness; null uses the adapter default."
  type        = string
  default     = null
}

variable "slack_workspace_id" {
  description = "Slack workspace (team) ID the app is installed in."
  type        = string
  default     = "T0FAKE"
}

variable "slack_endpoint_ref" {
  description = "Slack API base URL override, e.g. the fake Slack server. Empty uses slack.com."
  type        = string
  default     = ""
}

variable "linear_team_id" {
  description = "Linear team ID that agents create projects and issues in."
  type        = string
  default     = "TEAM-FAKE"
}

variable "linear_endpoint_ref" {
  description = "Linear API base URL override, e.g. the fake Linear server. Empty uses api.linear.app."
  type        = string
  default     = ""
}

variable "model_endpoint_ref" {
  description = "Model API base URL override. Empty uses the provider default."
  type        = string
  default     = ""
}

variable "secret_refs" {
  description = "secret_ref per connection (foundation output secret_refs). References only; values stay in the control plane."
  type = object({
    slack     = string
    linear    = string
    anthropic = string
  })
  default = {
    slack     = "k8s:slack-credentials"
    linear    = "k8s:linear-credentials"
    anthropic = "k8s:anthropic-credentials"
  }
}

variable "seat_idle_timeout" {
  type    = string
  default = "15m"
}

variable "wait_for_ready" {
  type    = bool
  default = true
}

variable "ready_timeout" {
  description = "Create/update/delete timeout."
  type        = string
  default     = "20m"
}
