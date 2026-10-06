-- +goose Up
-- Read paths of the console API (/console/v1): run history, the activity
-- feed and the work view.
CREATE INDEX execution_events_execution ON execution_events (execution_id, id);
CREATE INDEX execution_events_created ON execution_events (created_at);
CREATE INDEX executions_started ON executions (started_at);
CREATE INDEX executions_finished ON executions (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX messages_organization_created ON messages (organization_id, created_at);
CREATE INDEX connector_operations_updated ON connector_operations (organization_id, updated_at);
CREATE INDEX connector_operations_execution ON connector_operations (execution_id) WHERE execution_id IS NOT NULL;

-- +goose Down
DROP INDEX connector_operations_execution, connector_operations_updated, messages_organization_created,
    executions_finished, executions_started, execution_events_created, execution_events_execution;
