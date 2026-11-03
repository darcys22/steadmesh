-- +goose Up
-- Persist the exact approved startup route with its native checkpoint.
ALTER TABLE sessions ADD COLUMN harness_profile text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN route_binding text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN route_binding;
ALTER TABLE sessions DROP COLUMN harness_profile;
