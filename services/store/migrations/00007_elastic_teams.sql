-- +goose Up
CREATE TABLE elastic_teams (
 id uuid PRIMARY KEY,
 parent_seat_id uuid NOT NULL REFERENCES seats(id) ON DELETE CASCADE,
 parent_execution_id uuid NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
 request_id uuid NOT NULL,
 request jsonb NOT NULL,
 request_sha256 text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(parent_seat_id,request_id)
);
CREATE TABLE elastic_team_members (
 team_id uuid NOT NULL REFERENCES elastic_teams(id) ON DELETE CASCADE,
 task_run_id uuid NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
 name text NOT NULL,
 template text NOT NULL,
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 0 AND 63),
 PRIMARY KEY(team_id,task_run_id),
 UNIQUE(team_id,name),
 UNIQUE(team_id,ordinal)
);
CREATE INDEX elastic_teams_parent ON elastic_teams(parent_seat_id,created_at);
-- +goose Down
DROP TABLE elastic_team_members;
DROP TABLE elastic_teams;
