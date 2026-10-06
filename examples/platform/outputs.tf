output "release" {
  value = {
    name      = helm_release.platform.name
    namespace = helm_release.platform.namespace
    status    = helm_release.platform.status
    version   = helm_release.platform.version
  }
}

output "platform_url" {
  description = "Platform URL seen by seats and the controller."
  value       = "http://steadmesh-platform.${var.system_namespace}.svc:8080"
}

output "console" {
  description = "How to open the Steadmesh Console, when enable_console is set; null otherwise."
  value = var.enable_console ? {
    deployment   = "steadmesh-console"
    namespace    = var.system_namespace
    port_forward = "kubectl --context kind-steadmesh -n ${var.system_namespace} port-forward deploy/steadmesh-console 8090"
    url          = "http://127.0.0.1:8090"
  } : null
}
