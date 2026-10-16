-- +goose Up
ALTER TABLE task_runs ADD COLUMN launch_started_at timestamptz;
ALTER TABLE task_runs ADD COLUMN launch_resolved_at timestamptz;
ALTER TABLE task_runs ADD CONSTRAINT task_launch_resolved_requires_started CHECK (launch_resolved_at IS NULL OR launch_started_at IS NOT NULL);
-- +goose Down
ALTER TABLE task_runs DROP COLUMN launch_resolved_at;
ALTER TABLE task_runs DROP COLUMN launch_started_at;
