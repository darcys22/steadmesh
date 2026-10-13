-- +goose Up
-- An explicit shared handoff and independent alternatives, built on task_runs.
CREATE TABLE run_branches (
    id uuid PRIMARY KEY,
    parent_seat_id uuid NOT NULL REFERENCES seats(id) ON DELETE CASCADE,
    parent_execution_id uuid NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    request_id uuid NOT NULL,
    request jsonb NOT NULL,
    request_sha256 text NOT NULL,
    snapshot_sha256 text NOT NULL,
    revision integer NOT NULL DEFAULT 1 CHECK (revision > 0),
    selected_task_run_id uuid,
    selected_result_sha256 text,
    selected_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (parent_seat_id, request_id),
    CHECK ((selected_task_run_id IS NULL AND selected_result_sha256 IS NULL AND selected_at IS NULL)
        OR (selected_task_run_id IS NOT NULL AND selected_result_sha256 IS NOT NULL AND selected_at IS NOT NULL))
);
CREATE TABLE run_branch_members (
    branch_id uuid NOT NULL REFERENCES run_branches(id) ON DELETE CASCADE,
    task_run_id uuid NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
    name text NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 3),
    PRIMARY KEY (branch_id, task_run_id),
    UNIQUE (task_run_id),
    UNIQUE (branch_id, name),
    UNIQUE (branch_id, ordinal)
);
ALTER TABLE run_branches ADD CONSTRAINT run_branch_selected_member
    FOREIGN KEY (id, selected_task_run_id) REFERENCES run_branch_members(branch_id, task_run_id);
-- Choice history survives replacement of the current choice.
CREATE TABLE run_branch_selections (
    branch_id uuid NOT NULL,
    revision integer NOT NULL,
    task_run_id uuid NOT NULL,
    result_sha256 text NOT NULL,
    selected_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (branch_id, revision),
    FOREIGN KEY (branch_id, task_run_id) REFERENCES run_branch_members(branch_id, task_run_id) ON DELETE CASCADE
);
CREATE INDEX run_branches_parent ON run_branches (parent_seat_id, created_at);

-- +goose Down
DROP TABLE run_branch_selections;
ALTER TABLE run_branches DROP CONSTRAINT run_branch_selected_member;
DROP TABLE run_branch_members;
DROP TABLE run_branches;
