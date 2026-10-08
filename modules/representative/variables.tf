variable "human" {
  description = "Stable key of the human (lowercase letters, digits, '_'), e.g. sean. Also the channel binding key."
  type        = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9_]{0,40}$", var.human))
    error_message = "human must match ^[a-z][a-z0-9_]{0,40}$."
  }
}

variable "display_name" {
  description = "Seat display name, e.g. \"Sean's representative\"."
  type        = string
  default     = null
}

variable "external_user_id" {
  description = "Verified external user ID on the communication connection, e.g. a Slack user ID, or the terminal ID for a terminal connection."
  type        = string
}

variable "connection" {
  description = "Communication connection key in spec.connections."
  type        = string
  default     = "slack"
}

variable "role_ref" {
  description = "Representative role instruction reference (modules/instruction-bundle output for bundles/roles/representative.md)."
  type        = string
}

variable "instruction_refs" {
  description = "Additional seat-scoped instruction references."
  type        = list(string)
  default     = []
}

variable "harness_profile" {
  type    = string
  default = "primary"
}

variable "execution_profile" {
  type    = string
  default = "interactive"
}

variable "sandbox_profile" {
  type    = string
  default = "standard"
}

variable "seat_key" {
  description = "Override the seat key (default representative_<human>). Changing it later is a rename, not an edit."
  type        = string
  default     = null
}

variable "memory_store_key" {
  description = "Override the personal memory store key (default rep_<human>)."
  type        = string
  default     = null
}

variable "memory_retention" {
  description = "Retention of the private history: retain or delete."
  type        = string
  default     = "retain"
}

variable "timezone" {
  description = "The human's IANA time zone, e.g. Australia/Melbourne. Their representative reads and schedules times of day in it. Null uses the organisation's."
  type        = string
  default     = null
}
