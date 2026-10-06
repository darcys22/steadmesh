variable "namespace" {
  description = "Control-plane namespace the platform runs in."
  type        = string
}

variable "database_secret" {
  description = "Name of the Secret (key url) holding the connection URL; the platform chart's database.secretName."
  type        = string
  default     = "steadmesh-db"
}

variable "postgres_image" {
  type    = string
  default = "postgres:16.4-alpine"
}

variable "postgres_storage" {
  description = "Size of the data volume."
  type        = string
  default     = "5Gi"
}
