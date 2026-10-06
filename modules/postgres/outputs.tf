output "database_secret" {
  description = "Secret (key url) holding the Postgres connection URL."
  value       = kubernetes_secret_v1.db.metadata[0].name
}
