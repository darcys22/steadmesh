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
  description = "Harness for every seat not listed in seat_harnesses: claude-code, codex, pi or fake (deterministic, no model)."
  type        = string
  default     = "fake"
  validation {
    condition     = contains(["claude-code", "codex", "pi", "fake"], var.harness)
    error_message = "harness must be claude-code, codex, pi or fake."
  }
}

variable "model" {
  description = "Model for the default harness: connection (a key of model_connections), id, and optionally api and settings. Required unless harness is fake."
  type = object({
    connection = string
    id         = string
    api        = optional(string)
    settings   = optional(map(string))
  })
  default = null
}

variable "seat_harnesses" {
  description = "Per-seat harness and model, e.g. { engineer = { adapter = \"codex\", model = { connection = \"openai\", id = \"gpt-5.5\" } } }. Other seats use harness and model."
  type = map(object({
    adapter = string
    model = optional(object({
      connection = string
      id         = string
      api        = optional(string)
      settings   = optional(map(string))
    }))
  }))
  default = {}
}

variable "model_connections" {
  description = "Model endpoints, keyed by connection: adapter (anthropic, openai, or model for any compatible endpoint), secret_ref, and optionally endpoint_ref (the API base, usually ending in /v1) and model (apis, auth, models, verify). Only connections a seat uses are declared."
  type = map(object({
    adapter      = string
    secret_ref   = string
    endpoint_ref = optional(string)
    model = optional(object({
      apis   = optional(list(string))
      auth   = optional(string)
      verify = optional(string)
      models = optional(list(object({
        id   = string
        apis = optional(list(string))
      })))
    }))
  }))
  default = {
    anthropic = { adapter = "anthropic", secret_ref = "k8s:anthropic-credentials" }
    openai    = { adapter = "openai", secret_ref = "k8s:openai-credentials" }
  }
}

variable "harness_images" {
  description = "Pinned seat images per harness (image@sha256:... in production; local tags in kind)."
  type        = map(string)
  default = {
    "claude-code" = "steadmesh/seat-claudecode:dev"
    "codex"       = "steadmesh/seat-codex:dev"
    "pi"          = "steadmesh/seat-pi:dev"
    "fake"        = "steadmesh/seat-fake:dev"
  }
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

variable "secret_refs" {
  description = "secret_ref per connection (foundation output secret_refs). References only; values stay in the control plane."
  type = object({
    slack  = string
    linear = string
  })
  default = {
    slack  = "k8s:slack-credentials"
    linear = "k8s:linear-credentials"
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

variable "publish_work_to_tracker" {
  description = "Publish engineering work items to the Linear connection for people to follow. Optional: coordination never depends on it."
  type        = bool
  default     = false
}

variable "enable_linear" {
  description = "Declare the Linear connection. Without it the organisation coordinates purely through Steadmesh memory and messages."
  type        = bool
  default     = true
}
