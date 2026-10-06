output "ref" {
  description = "Instruction reference: configmap:<name>/<key>#sha256:<digest of content>."
  # Referencing the resource orders the organisation after the ConfigMap.
  value = "configmap:${kubernetes_config_map_v1.this.metadata[0].name}/${local.key}#sha256:${local.digest}"
}

output "sha256" {
  description = "SHA-256 of the bundle content."
  value       = local.digest
}

output "name" {
  value = kubernetes_config_map_v1.this.metadata[0].name
}

output "key" {
  value = local.key
}
