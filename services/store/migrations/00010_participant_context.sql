-- +goose Up
CREATE TABLE participant_context (
 seat_id uuid PRIMARY KEY REFERENCES seats(id),
 revision integer NOT NULL CHECK (revision >= 1),
 instructions text NOT NULL DEFAULT '' CHECK (octet_length(instructions) <= 4096),
 summary text NOT NULL DEFAULT '' CHECK (octet_length(summary) <= 4096),
 actor_seat_id uuid NOT NULL REFERENCES seats(id),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (actor_seat_id = seat_id)
);
CREATE TABLE participant_context_history (
 seat_id uuid NOT NULL REFERENCES seats(id),
 revision integer NOT NULL CHECK (revision >= 1),
 instructions text NOT NULL CHECK (octet_length(instructions) <= 4096),
 summary text NOT NULL CHECK (octet_length(summary) <= 4096),
 actor_seat_id uuid NOT NULL REFERENCES seats(id),
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (seat_id, revision),
 CHECK (actor_seat_id = seat_id)
);
-- +goose Down
DROP TABLE participant_context_history;
DROP TABLE participant_context;
