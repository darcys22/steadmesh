-- +goose Up
-- Durable operational store (design §13.1). Every table is scoped by
-- organisation; uniqueness and optimistic concurrency are database-enforced.

CREATE TABLE organizations (
    id              uuid PRIMARY KEY,
    key             text NOT NULL,
    namespace       text NOT NULL,
    source_uid      text NOT NULL,
    active_revision text NOT NULL DEFAULT '',
    manifest        jsonb NOT NULL DEFAULT '{}'::jsonb,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (namespace, key)
);

CREATE TABLE seats (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    key             text NOT NULL,
    source_uid      text NOT NULL DEFAULT '',
    service_account text NOT NULL,
    config_revision text NOT NULL DEFAULT '',
    -- capability manifest (compile.SeatManifest) of the active policy revision
    manifest        jsonb NOT NULL DEFAULT '{}'::jsonb,
    policy_revision bigint NOT NULL DEFAULT 1,
    retired_at      timestamptz,
    adopted_from    uuid REFERENCES seats(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
-- One active seat per key per organisation; retired seats keep their rows.
CREATE UNIQUE INDEX seats_active_key ON seats (organization_id, key) WHERE retired_at IS NULL;
CREATE UNIQUE INDEX seats_service_account ON seats (organization_id, service_account) WHERE retired_at IS NULL;

CREATE TABLE execution_leases (
    seat_id      uuid PRIMARY KEY REFERENCES seats(id),
    generation   bigint NOT NULL DEFAULT 0,
    holder       text NOT NULL DEFAULT '',
    expires_at   timestamptz NOT NULL DEFAULT 'epoch',
    state        text NOT NULL DEFAULT 'Stopped',
    state_detail text NOT NULL DEFAULT '',
    last_activity timestamptz,
    adopted_revision text NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE conversations (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    -- kind: human (external channel thread) or internal (seat to seat)
    kind            text NOT NULL,
    -- for human conversations: binding key and external channel id
    binding         text NOT NULL DEFAULT '',
    external_ref    text NOT NULL DEFAULT '',
    opened_by       uuid REFERENCES seats(id),
    -- seats allowed to read this conversation's history
    participants    uuid[] NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX conversations_external ON conversations (organization_id, binding, external_ref) WHERE kind = 'human';

CREATE TABLE messages (
    id                uuid PRIMARY KEY,
    organization_id   uuid NOT NULL REFERENCES organizations(id),
    conversation_id   uuid NOT NULL REFERENCES conversations(id),
    -- origin: human, seat, system, schedule, probe. Assigned by trusted ingress only.
    origin            text NOT NULL,
    origin_connection text NOT NULL DEFAULT '',
    origin_external_user text NOT NULL DEFAULT '',
    sender_seat_id    uuid REFERENCES seats(id),
    recipient_seat_id uuid REFERENCES seats(id),
    -- outbound to a human: binding key
    recipient_binding text NOT NULL DEFAULT '',
    parent_id         uuid REFERENCES messages(id),
    correlation_id    uuid,
    external_event_id text,
    body              text NOT NULL,
    artifact_id       uuid,
    route             text NOT NULL DEFAULT '',
    sender_generation bigint,
    created_at        timestamptz NOT NULL DEFAULT now()
);
-- Deduplicate external deliveries by source installation and event id (§9.2, A09).
CREATE UNIQUE INDEX messages_external_event ON messages (organization_id, origin_connection, external_event_id) WHERE external_event_id IS NOT NULL;
CREATE INDEX messages_conversation ON messages (conversation_id, created_at);

-- Per-seat inbox (§9.2).
CREATE TABLE deliveries (
    id             bigserial PRIMARY KEY,
    message_id     uuid NOT NULL REFERENCES messages(id),
    seat_id        uuid NOT NULL REFERENCES seats(id),
    -- pending, leased, done, dead; passive: queued without starting a turn,
    -- handed over with the seat's next turn (messages.send wake=false)
    state          text NOT NULL DEFAULT 'pending',
    attempts       int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_generation bigint,
    leased_until   timestamptz,
    last_error     text NOT NULL DEFAULT '',
    execution_id   uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, seat_id)
);
CREATE INDEX deliveries_pending ON deliveries (seat_id, state, next_attempt_at);

-- Transactional outbox for external delivery (replies to humans).
CREATE TABLE outbox (
    id              bigserial PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    message_id      uuid NOT NULL UNIQUE REFERENCES messages(id),
    connection      text NOT NULL,
    -- pending, sent, unknown, dead
    state           text NOT NULL DEFAULT 'pending',
    attempts        int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    operation_id    uuid,
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_pending ON outbox (state, next_attempt_at);

CREATE TABLE memory_stores (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    key             text NOT NULL,
    retention       text NOT NULL,
    -- personal store owner, if any
    owner_seat_id   uuid REFERENCES seats(id),
    policy_revision bigint NOT NULL DEFAULT 1,
    -- last work item number allocated in this store (work ids <store>/W-<n>)
    work_seq        bigint NOT NULL DEFAULT 0,
    retired_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
-- A retired memory store keeps its rows (retention) but frees its key, so a
-- store re-declared under the same key starts empty instead of silently
-- re-granting retained private data to a new seat identity (§4.1, A18).
CREATE UNIQUE INDEX memory_stores_active_key ON memory_stores (organization_id, key) WHERE retired_at IS NULL;

-- array_to_string is only STABLE; generated columns need an immutable expression.
-- +goose StatementBegin
CREATE FUNCTION memory_tags_text(tags text[]) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT array_to_string(tags, ' ') $$;
-- +goose StatementEnd

CREATE TABLE memory_records (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    store_id        uuid NOT NULL REFERENCES memory_stores(id),
    revision        int NOT NULL,
    -- path within the store, like a file path (notes/plan.md, work/W-3.md);
    -- unique among the store's live records.
    path            text NOT NULL,
    -- note: free-form text any writer may change. work: a structured work
    -- item whose data is changed only through the work tools, which enforce
    -- ownership and status rules; its body is rendered from data.
    kind            text NOT NULL DEFAULT 'note' CHECK (kind IN ('note', 'work')),
    data            jsonb,
    body            text NOT NULL,
    tags            text[] NOT NULL DEFAULT '{}',
    author_seat_id  uuid NOT NULL REFERENCES seats(id),
    source_refs     text[] NOT NULL DEFAULT '{}',
    archived        boolean NOT NULL DEFAULT false,
    search          tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', path), 'A') ||
        setweight(to_tsvector('english', memory_tags_text(tags)), 'B') ||
        setweight(to_tsvector('english', body), 'C')) STORED,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX memory_records_store ON memory_records (organization_id, store_id) WHERE NOT archived;
CREATE UNIQUE INDEX memory_records_path ON memory_records (store_id, path) WHERE NOT archived;
CREATE INDEX memory_records_work_owner ON memory_records (organization_id, (data->>'owner')) WHERE kind = 'work' AND NOT archived;
CREATE INDEX memory_records_search ON memory_records USING gin (search);

-- Append-only revision history (§8.3).
CREATE TABLE memory_revisions (
    record_id       uuid NOT NULL REFERENCES memory_records(id),
    revision        int NOT NULL,
    store_id        uuid NOT NULL REFERENCES memory_stores(id),
    path            text NOT NULL,
    data            jsonb,
    body            text NOT NULL,
    tags            text[] NOT NULL DEFAULT '{}',
    author_seat_id  uuid NOT NULL REFERENCES seats(id),
    author_generation bigint,
    source_refs     text[] NOT NULL DEFAULT '{}',
    archived        boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (record_id, revision)
);

CREATE TABLE executions (
    id              uuid PRIMARY KEY,
    seat_id         uuid NOT NULL REFERENCES seats(id),
    lease_generation bigint NOT NULL,
    config_revision text NOT NULL,
    harness_adapter text NOT NULL,
    trigger_message_id uuid REFERENCES messages(id),
    -- running, completed, failed, interrupted
    state           text NOT NULL DEFAULT 'running',
    error           text NOT NULL DEFAULT '',
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX executions_seat ON executions (seat_id, started_at DESC);

CREATE TABLE execution_events (
    id            bigserial PRIMARY KEY,
    execution_id  uuid NOT NULL REFERENCES executions(id),
    kind          text NOT NULL,
    correlation_id text NOT NULL DEFAULT '',
    data          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    seat_id         uuid NOT NULL REFERENCES seats(id),
    harness_adapter text NOT NULL,
    format_version  text NOT NULL,
    checkpoint_ref  text NOT NULL,
    -- application_checkpoint | process_snapshot
    guarantee       text NOT NULL,
    lease_generation bigint NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (seat_id, harness_adapter)
);

-- Portable execution handoff (§8.4).
CREATE TABLE handoffs (
    seat_id         uuid PRIMARY KEY REFERENCES seats(id),
    objective       text NOT NULL DEFAULT '',
    unresolved      text[] NOT NULL DEFAULT '{}',
    record_ids      uuid[] NOT NULL DEFAULT '{}',
    pending_message_ids uuid[] NOT NULL DEFAULT '{}',
    operation_ids   uuid[] NOT NULL DEFAULT '{}',
    notes           text NOT NULL DEFAULT '',
    lease_generation bigint NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Connector operation ledger (§10.3).
CREATE TABLE connector_operations (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    seat_id         uuid REFERENCES seats(id),
    execution_id    uuid,
    lease_generation bigint,
    connection      text NOT NULL,
    operation       text NOT NULL,
    target          text NOT NULL DEFAULT '',
    request_hash    text NOT NULL,
    idempotency_key text NOT NULL,
    -- pending, succeeded, failed, unknown
    status          text NOT NULL DEFAULT 'pending',
    attempts        int NOT NULL DEFAULT 0,
    external_receipt text NOT NULL DEFAULT '',
    result          jsonb,
    error           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, connection, idempotency_key)
);

CREATE TABLE wake_schedules (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    seat_id         uuid NOT NULL REFERENCES seats(id),
    -- RFC3339 one-off time or interval (e.g. "@every 1h")
    schedule        text NOT NULL,
    next_trigger_at timestamptz NOT NULL,
    note            text NOT NULL DEFAULT '',
    author_seat_id  uuid NOT NULL REFERENCES seats(id),
    last_fired_at   timestamptz,
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wake_schedules_due ON wake_schedules (next_trigger_at) WHERE active;

CREATE TABLE artifacts (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    store_id        uuid REFERENCES memory_stores(id),
    owner_seat_id   uuid REFERENCES seats(id),
    location        text NOT NULL,
    content_digest  text NOT NULL,
    size_bytes      bigint NOT NULL,
    retention       text NOT NULL DEFAULT 'retain',
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Connection verification results (readiness, §5.4).
CREATE TABLE connection_checks (
    organization_id uuid NOT NULL REFERENCES organizations(id),
    connection      text NOT NULL,
    ok              boolean NOT NULL,
    detail          text NOT NULL DEFAULT '',
    -- Credential refresh state (non-secret): runtimeapi.CredentialStatus.
    credential_state   text NOT NULL DEFAULT '',
    secret_version     text NOT NULL DEFAULT '',
    credential_error   text NOT NULL DEFAULT '',
    refreshed_at       timestamptz,
    previous_until     timestamptz,
    refresh_failing_since timestamptz,
    checked_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, connection)
);

-- Wake-up notification for the controller (LISTEN steadmesh_wake).
-- +goose StatementBegin
CREATE FUNCTION notify_delivery() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('steadmesh_wake', NEW.seat_id::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- Passive deliveries must not wake anyone.
CREATE TRIGGER deliveries_notify AFTER INSERT ON deliveries FOR EACH ROW WHEN (NEW.state <> 'passive')
    EXECUTE FUNCTION notify_delivery();

-- Read paths of the console API (/console/v1): run history, the activity
-- feed and the work view.
CREATE INDEX execution_events_execution ON execution_events (execution_id, id);
CREATE INDEX execution_events_created ON execution_events (created_at);
CREATE INDEX executions_started ON executions (started_at);
CREATE INDEX executions_finished ON executions (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX messages_organization_created ON messages (organization_id, created_at);
CREATE INDEX connector_operations_updated ON connector_operations (organization_id, updated_at);
CREATE INDEX connector_operations_execution ON connector_operations (execution_id) WHERE execution_id IS NOT NULL;

-- Optional outward publication of work items to a tracker (work_publication).
-- The row exists before the external object is created; its stable creation
-- key lets a crash between creation and saving external_id converge by
-- read-back instead of creating a duplicate.
CREATE TABLE work_publications (
    organization_id    uuid NOT NULL REFERENCES organizations(id),
    record_id          uuid NOT NULL REFERENCES memory_records(id),
    connection         text NOT NULL,
    external_kind      text NOT NULL,
    external_id        text NOT NULL DEFAULT '',
    external_url       text NOT NULL DEFAULT '',
    published_revision int NOT NULL DEFAULT 0,
    -- pending, published, blocked (connection unavailable; retried), failed
    state              text NOT NULL DEFAULT 'pending',
    attempts           int NOT NULL DEFAULT 0,
    next_attempt_at    timestamptz NOT NULL DEFAULT now(),
    last_error         text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, record_id, connection, external_kind)
);
CREATE INDEX work_publications_due ON work_publications (next_attempt_at) WHERE state <> 'published';

-- +goose Down
DROP TRIGGER deliveries_notify ON deliveries;
DROP FUNCTION notify_delivery();
DROP TABLE work_publications, connection_checks, artifacts, wake_schedules, connector_operations, handoffs, sessions,
    execution_events, executions, memory_revisions, memory_records, memory_stores, outbox,
    deliveries, messages, conversations, execution_leases, seats, organizations;
DROP FUNCTION memory_tags_text(text[]);
