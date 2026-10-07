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
    slack     = local.create.slack ? "k8s:${kubernetes_secret_v1.slack[0].metadata[0].name}" : var.existing_secret_refs.slack
    anthropic = local.create.anthropic ? "k8s:${kubernetes_secret_v1.anthropic[0].metadata[0].name}" : var.existing_secret_refs.anthropic
    linear    = local.create.linear ? "k8s:${kubernetes_secret_v1.linear[0].metadata[0].name}" : var.existing_secret_refs.linear
  }
}

output "console" {
  value = module.steadmesh.console
}
