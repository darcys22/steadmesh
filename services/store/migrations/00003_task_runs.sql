-- +goose Up
CREATE TABLE task_runs (
    id uuid PRIMARY KEY,
    parent_seat_id uuid NOT NULL REFERENCES seats(id) ON DELETE CASCADE,
    parent_execution_id uuid NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    parent_generation bigint NOT NULL,
    config_revision text NOT NULL,
    service_account text NOT NULL UNIQUE,
    request_id uuid NOT NULL,
    request jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','completed','failed','cancelled','timed_out')),
    pod_uid text NOT NULL DEFAULT '',
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    deadline timestamptz NOT NULL,
    finished_at timestamptz,
    cleaned_at timestamptz,
    UNIQUE (parent_seat_id, request_id)
);
CREATE INDEX task_runs_parent ON task_runs (parent_seat_id, created_at DESC);
CREATE INDEX task_runs_cleanup ON task_runs (parent_seat_id) WHERE cleaned_at IS NULL;

-- +goose Down
DROP TABLE task_runs;
