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
    slack  = local.create.slack ? "k8s:${kubernetes_secret_v1.slack[0].metadata[0].name}" : var.existing_secret_refs.slack
    linear = local.create.linear ? "k8s:${kubernetes_secret_v1.linear[0].metadata[0].name}" : var.existing_secret_refs.linear
    models = merge(var.existing_secret_refs.models, { for k, s in kubernetes_secret_v1.model : k => "k8s:${s.metadata[0].name}" })
  }
}

output "console" {
  value = module.steadmesh.console
}
