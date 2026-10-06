# Read by the organisation stage (../organisation) through its local state.

output "kube_context" {
  value = var.kube_context
}

output "kubeconfig_path" {
  value = var.kubeconfig_path
}

output "organisation_namespace" {
  value = module.steadmesh.organisation_namespace
}

output "seat_images" {
  value = module.steadmesh.seat_images
}

output "secret_refs" {
  description = "secret_ref per connection; references only."
  value = {
    slack     = "k8s:${kubernetes_secret_v1.slack.metadata[0].name}"
    anthropic = "k8s:${kubernetes_secret_v1.anthropic.metadata[0].name}"
    linear    = var.linear_api_key == null ? null : "k8s:${kubernetes_secret_v1.linear[0].metadata[0].name}"
  }
}

output "console" {
  value = module.steadmesh.console
}
