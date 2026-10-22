-- +goose Up
-- Agent runtime choices deliberately do not live in the Terraform manifest.
CREATE TABLE seat_skill_selections (
 seat_id uuid NOT NULL REFERENCES seats(id),
 marketplace text NOT NULL CHECK (octet_length(marketplace) BETWEEN 1 AND 128),
 plugin text NOT NULL CHECK (octet_length(plugin) BETWEEN 1 AND 128),
 skill text NOT NULL CHECK (octet_length(skill) BETWEEN 1 AND 128),
 repository text NOT NULL,
 source_ref text NOT NULL,
 source_fingerprint text NOT NULL CHECK (length(source_fingerprint) = 64),
 selected_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (seat_id, marketplace, plugin, skill)
);
-- +goose Down
DROP TABLE seat_skill_selections;
