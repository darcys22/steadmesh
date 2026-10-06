output "organization_id" {
  value = steadmesh_organization.this.id
}

output "connection_details" {
  description = "Non-secret connection details: representatives and the humans they are bound to."
  value       = steadmesh_organization.this.connection_details
}

output "resolved_seats" {
  value = steadmesh_organization.this.resolved_seats
}

output "conditions" {
  value = steadmesh_organization.this.conditions
}
