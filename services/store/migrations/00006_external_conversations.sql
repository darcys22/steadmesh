-- +goose Up
-- External agents have dedicated conversations, separate from human bindings.
-- The immutable representative seat identity prevents a recreated seat from
-- inheriting an earlier seat's conversations or idempotency records.
CREATE TABLE external_conversations (
    conversation_id uuid PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    representative_seat_id uuid NOT NULL REFERENCES seats(id),
    caller_id text NOT NULL CHECK (caller_id <> '')
);
CREATE INDEX external_conversations_principal ON external_conversations
    (organization_id, representative_seat_id, caller_id);

ALTER TABLE messages ADD COLUMN origin_external_caller text NOT NULL DEFAULT '';
-- Allocated only after locking the representative for external writes, so
-- pagination follows commit order within each external conversation.
CREATE SEQUENCE external_message_sequence;
ALTER TABLE messages ADD COLUMN external_sequence bigint;
CREATE INDEX messages_external_sequence ON messages (conversation_id, external_sequence)
    WHERE external_sequence IS NOT NULL;

CREATE TABLE external_requests (
    message_id uuid PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    representative_seat_id uuid NOT NULL REFERENCES seats(id),
    caller_id text NOT NULL CHECK (caller_id <> ''),
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    -- Preserve the original input, including an omitted conversation, so a
    -- retry cannot quietly change the conversation associated with its key.
    requested_conversation_id uuid,
    UNIQUE (organization_id, representative_seat_id, caller_id, idempotency_key)
);

-- +goose Down
DROP TABLE external_requests;
DROP INDEX messages_external_sequence;
ALTER TABLE messages DROP COLUMN external_sequence;
DROP SEQUENCE external_message_sequence;
ALTER TABLE messages DROP COLUMN origin_external_caller;
DROP TABLE external_conversations;
