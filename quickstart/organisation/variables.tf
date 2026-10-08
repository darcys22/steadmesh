variable "organisation_key" {
  description = "Stable key of the organisation. Changing it creates a new organisation."
  type        = string
  default     = "my-company"
}

variable "display_name" {
  type    = string
  default = "My Company"
}

variable "humans" {
  description = <<-EOT
    People who get a personal representative: key => how they chat and the
    representative's display name. channel is slack (set slack_user_id, the
    member ID U...) or terminal (chat with orgctl chat --user <key>; list the
    key in the platform stage's terminal_users). timezone is their IANA time
    zone, e.g. Australia/Melbourne; it defaults to the organisation's.
  EOT
  type = map(object({
    channel       = optional(string, "slack")
    slack_user_id = optional(string)
    display_name  = optional(string)
    timezone      = optional(string)
  }))
  validation {
    condition     = alltrue([for h in var.humans : contains(["slack", "terminal"], h.channel)])
    error_message = "channel must be slack or terminal."
  }
  validation {
    condition     = alltrue([for h in var.humans : h.channel != "slack" || h.slack_user_id != null])
    error_message = "Humans on Slack need slack_user_id."
  }
}

variable "timezone" {
  description = "IANA time zone in which agents read and schedule times of day, e.g. Australia/Melbourne. A human's own timezone in humans overrides it for their representative."
  type        = string
  default     = "UTC"
}

variable "slack_workspace_id" {
  description = "ID of the Slack workspace (team) the app is installed in (T...). Needed when any human chats over Slack."
  type        = string
  default     = null
}

variable "linear_team_id" {
  description = "Linear team agents create projects and issues in. Used only when the platform stage has a Linear API key."
  type        = string
  default     = null
}

variable "harness" {
  description = "Harness for every seat not listed in seat_harnesses: claude-code, codex or pi (fake is for testing)."
  type        = string
  default     = "claude-code"
}

variable "model" {
  description = "Model for the default harness: connection (a key of model_connections), id, and optionally api and settings (see the harness documentation)."
  type = object({
    connection = string
    id         = string
    api        = optional(string)
    settings   = optional(map(string))
  })
  default = { connection = "anthropic", id = "claude-sonnet-5-5" }
}

variable "seat_harnesses" {
  description = "Per-seat harness and model, e.g. { engineer = { adapter = \"codex\", model = { connection = \"openai\", id = \"gpt-5.5\" } } }."
  type = map(object({
    adapter = string
    model = object({
      connection = string
      id         = string
      api        = optional(string)
      settings   = optional(map(string))
    })
  }))
  default = {}
}

variable "model_connections" {
  description = "Model endpoints, keyed like the platform stage's model_api_keys: adapter (anthropic, openai, or model for any compatible endpoint), and for the model adapter endpoint_ref (API base, e.g. https://host/v1) and model (apis, models, auth). Only connections a seat uses are declared."
  type = map(object({
    adapter      = string
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
    anthropic = { adapter = "anthropic" }
    openai    = { adapter = "openai" }
  }
}

variable "seat_idle_timeout" {
  description = "How long an idle seat stays warm before it is stopped. Its workspace and memory are kept."
  type        = string
  default     = "15m"
}

# ---- Advanced: used to test against fake services ---------------------------

variable "slack_endpoint_ref" {
  description = "Slack API base URL override; null uses slack.com."
  type        = string
  default     = null
}

variable "linear_endpoint_ref" {
  description = "Linear API base URL override; null uses linear.app."
  type        = string
  default     = null
}

variable "publish_work_to_linear" {
  description = "When Linear is configured, publish engineering work items there for people to follow. Agents coordinate in Steadmesh memory and messages either way."
  type        = bool
  default     = false
}
