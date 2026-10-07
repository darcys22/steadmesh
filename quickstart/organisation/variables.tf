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
  description = "People who get a personal representative: key => Slack member ID (U...) and the representative's display name."
  type = map(object({
    slack_user_id = string
    display_name  = optional(string)
  }))
}

variable "slack_workspace_id" {
  description = "ID of the Slack workspace (team) the app is installed in (T...)."
  type        = string
}

variable "linear_team_id" {
  description = "Linear team agents create projects and issues in. Used only when the platform stage has a Linear API key."
  type        = string
  default     = null
}

variable "model" {
  description = "Claude model for every seat; null uses the harness default."
  type        = string
  default     = null
}

variable "seat_idle_timeout" {
  description = "How long an idle seat stays warm before it is stopped. Its workspace and memory are kept."
  type        = string
  default     = "15m"
}

# ---- Advanced: used to test against fake services ---------------------------

variable "harness" {
  description = "claude-code (real model) or fake (scripted, for testing)."
  type        = string
  default     = "claude-code"
}

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
