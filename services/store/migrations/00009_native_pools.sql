-- +goose Up
-- Native participants are ordinary independently fenced seats. Membership is
-- runtime state and is deliberately separate from the declarative manifest.
CREATE TABLE native_pool_members (
 seat_id uuid PRIMARY KEY REFERENCES seats(id) ON DELETE CASCADE,
 organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 team_key text NOT NULL,
 template_key text NOT NULL,
 requested_by uuid REFERENCES seats(id),
 request_id uuid,
 revoked_at timestamptz,
 retention_seconds integer NOT NULL DEFAULT 0 CHECK(retention_seconds>=0),
 retained_until timestamptz,
 purged_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(requested_by, request_id)
);
CREATE INDEX native_pool_members_team ON native_pool_members(organization_id,team_key);
-- +goose Down
DROP TABLE native_pool_members;
