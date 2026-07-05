-- +goose Up
-- Point-in-time circuit-breaker observations: append-only, so only
-- observed_at and no updated_at trigger.
CREATE TABLE backend_health_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    backend_id uuid NOT NULL REFERENCES model_backends(id) ON DELETE CASCADE,
    circuit_state text NOT NULL CHECK (circuit_state IN ('closed', 'open', 'half_open')),
    in_flight integer NOT NULL CHECK (in_flight >= 0),
    observed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_backend_health_snapshots_backend_id ON backend_health_snapshots (backend_id);

-- scope holds strings like 'tenant:<id>:key:<id>:POST:/v1/chat/completions'
-- so one Idempotency-Key cannot collide across tenants, keys, or routes.
CREATE TABLE idempotency_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope text NOT NULL,
    idem_key text NOT NULL,
    request_hash text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'completed')),
    locked_until timestamptz NULL,
    response_status integer NULL,
    response_headers jsonb NULL,
    response_body jsonb NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE UNIQUE INDEX uq_idempotency_records_scope_key ON idempotency_records (scope, idem_key);

-- +goose Down
DROP TABLE IF EXISTS idempotency_records;
DROP TABLE IF EXISTS backend_health_snapshots;
