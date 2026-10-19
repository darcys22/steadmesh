-- +goose Up
ALTER TABLE participant_context ADD COLUMN approver_seat_id uuid REFERENCES seats(id);
ALTER TABLE participant_context_history ADD COLUMN approver_seat_id uuid REFERENCES seats(id);
CREATE TABLE context_instruction_proposals (
 id uuid PRIMARY KEY,
 sequence bigserial UNIQUE NOT NULL,
 organization_id uuid NOT NULL REFERENCES organizations(id),
 owner_seat_id uuid NOT NULL REFERENCES seats(id),
 instructions text NOT NULL CHECK(octet_length(instructions)<=4096),
 reason text NOT NULL CHECK(octet_length(reason)<=1024),
 expected_context_revision integer NOT NULL CHECK(expected_context_revision>=0),
 policy_token text NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
 reviewer_seat_id uuid REFERENCES seats(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 decided_at timestamptz,
 CHECK(reviewer_seat_id IS NULL OR reviewer_seat_id<>owner_seat_id)
);
CREATE INDEX context_instruction_proposals_owner ON context_instruction_proposals(owner_seat_id,sequence DESC);
-- +goose Down
DROP TABLE context_instruction_proposals;
ALTER TABLE participant_context_history DROP COLUMN approver_seat_id;
ALTER TABLE participant_context DROP COLUMN approver_seat_id;
