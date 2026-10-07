# Import an organisation into this configuration by namespace/key.
terraform import steadmesh_organization.acme acme-org/acme

# Adopt an AgentOrganization that has no declaration owner yet.
terraform import steadmesh_organization.acme acme-org/acme/adopt
