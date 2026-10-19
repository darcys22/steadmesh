variable "kubeconfig_path" {
  type    = string
  default = "~/.kube/config"
}
variable "namespace" {
  type    = string
  default = "steadmesh-example"
}
variable "wait_for_ready" {
  type    = bool
  default = true
}
variable "harness_image" {
  description = "Local fake harness image loaded by make kind-load TAG=dev. Pin a digest for deployment."
  type        = string
  default     = "steadmesh/seat-fake:dev"
}
