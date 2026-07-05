-- +goose Up
-- Append-only request ledger: rows are written once at completion, so there
-- is no updated_at and no trigger. backend_id is SET NULL on delete to keep
-- history when a backend is retired.
CREATE TABLE inference_requests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    model text NOT NULL,
    backend_id uuid NULL REFERENCES model_backends(id) ON DELETE SET NULL,
    cache_result text NULL CHECK (cache_result IN ('hit', 'miss', 'bypass')),
    status text NOT NULL CHECK (status IN ('succeeded', 'failed')),
    latency_ms integer NOT NULL CHECK (latency_ms >= 0),
    request_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Per-tenant usage queries scan by recency.
CREATE INDEX idx_inference_requests_tenant_created ON inference_requests (tenant_id, created_at);
-- FK-support indexes so parent deletes don't sequential-scan this table.
CREATE INDEX idx_inference_requests_api_key_id ON inference_requests (api_key_id);
CREATE INDEX idx_inference_requests_backend_id ON inference_requests (backend_id);

-- +goose Down
DROP TABLE IF EXISTS inference_requests;
