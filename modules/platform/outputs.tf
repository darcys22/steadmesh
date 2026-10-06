output "system_namespace" {
  value = helm_release.platform.namespace
}

output "organisation_namespace" {
  value = kubernetes_namespace_v1.organisation.metadata[0].name
}

output "seat_images" {
  description = "Seat image per harness adapter, for harness_profiles image_digest. Pin a digest in production."
  value = {
    "claude-code" = "${local.images.seat_claudecode}:${local.tag}"
    "fake"        = "${local.images.seat_fake}:${local.tag}"
  }
}

output "console" {
  description = "How to open the console, when enable_console is set; null otherwise."
  value = var.enable_console ? {
    port_forward = "kubectl -n ${helm_release.platform.namespace} port-forward deploy/steadmesh-console 8090"
    url          = "http://127.0.0.1:8090"
  } : null
}
