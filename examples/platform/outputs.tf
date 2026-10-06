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
