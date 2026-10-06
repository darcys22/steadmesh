output "representatives" {
  description = "Who can message which representative."
  value       = steadmesh_organization.this.connection_details
}

output "conditions" {
  value = steadmesh_organization.this.conditions
}
