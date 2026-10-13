-- +goose Up
-- Reservations are charged before dispatch, without holding a transaction open
-- during inference. An abandoned reservation stays charged (fail closed).
CREATE TABLE model_budget_counters (
    organization_id uuid NOT NULL REFERENCES organizations(id),
    scope text NOT NULL CHECK (scope IN ('organization', 'seat', 'execution')),
    scope_id text NOT NULL,
    period_start date NOT NULL,
    charged_output_tokens bigint NOT NULL DEFAULT 0 CHECK (charged_output_tokens >= 0),
    PRIMARY KEY (organization_id, scope, scope_id, period_start)
);
CREATE TABLE model_budget_reservations (
    id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    seat_id uuid NOT NULL REFERENCES seats(id),
    execution_id uuid REFERENCES executions(id),
    day date NOT NULL,
    reserved_output_tokens bigint NOT NULL CHECK (reserved_output_tokens > 0),
    charged_output_tokens bigint NOT NULL CHECK (charged_output_tokens >= 0),
    settled boolean NOT NULL DEFAULT false,
    usage_known boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz
);
CREATE INDEX model_budget_reservations_org ON model_budget_reservations (organization_id, created_at);

-- +goose Down
DROP TABLE model_budget_reservations;
DROP TABLE model_budget_counters;
