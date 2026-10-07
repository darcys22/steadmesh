output "system_namespace" {
  value = kubernetes_namespace_v1.system.metadata[0].name
}

output "organisation_namespace" {
  value = kubernetes_namespace_v1.organisation.metadata[0].name
}

output "database_secret" {
  description = "Secret (key url) holding the Postgres connection URL."
  value       = module.postgres.database_secret
}

output "secret_refs" {
  description = "secret_ref values for the organisation's connections."
  value = {
    slack  = "k8s:${kubernetes_secret_v1.slack.metadata[0].name}"
    linear = "k8s:${kubernetes_secret_v1.linear.metadata[0].name}"
    models = { for k, s in kubernetes_secret_v1.model : k => "k8s:${s.metadata[0].name}" }
  }
}

output "vault_address" {
  description = "In-cluster Vault address when enable_vault is true."
  value       = var.enable_vault ? "http://vault.${var.system_namespace}.svc:8200" : null
}
