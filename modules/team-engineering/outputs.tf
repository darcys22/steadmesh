output "template_key" {
  value = var.template_key
}

output "team_templates" {
  description = "Entries for spec.team_templates (merge with the base module's)."
  value       = { (var.template_key) = local.template }
}

output "roles" {
  description = "Role key to instruction reference (engineering_lead, engineer, reviewer)."
  value       = local.roles
}

output "role_keys" {
  description = "Role keys usable as role_ref = \"role:<key>\" by seats on a team using this template."
  value       = sort(keys(local.roles))
}
