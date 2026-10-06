output "template_key" {
  description = "Key to use as `extends` in derived templates."
  value       = var.template_key
}

output "team_templates" {
  description = "Entries for spec.team_templates."
  value       = { (var.template_key) = local.template }
}

output "roles" {
  description = "Role key to instruction reference."
  value       = var.roles
}
