-- +goose Up
CREATE TABLE team_missions (
 organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 team_key text NOT NULL,
 revision bigint NOT NULL,
 policy_mission text NOT NULL,
 mission text NOT NULL,
 author_seat_id uuid NOT NULL REFERENCES seats(id),
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (organization_id, team_key, revision)
);
ALTER TABLE messages ADD COLUMN execution_id uuid REFERENCES executions(id) ON DELETE SET NULL;
CREATE INDEX messages_execution ON messages(execution_id) WHERE execution_id IS NOT NULL;
-- +goose Down
DROP INDEX messages_execution;
ALTER TABLE messages DROP COLUMN execution_id;
DROP TABLE team_missions;
