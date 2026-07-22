-- +goose Up
CREATE TABLE batch_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    model text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'partially_failed', 'failed')),
    total_items integer NOT NULL DEFAULT 0 CHECK (total_items >= 0),
    completed_items integer NOT NULL DEFAULT 0 CHECK (completed_items >= 0),
    failed_items integer NOT NULL DEFAULT 0 CHECK (failed_items >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_batch_jobs_tenant_id ON batch_jobs (tenant_id);
CREATE INDEX idx_batch_jobs_api_key_id ON batch_jobs (api_key_id);

CREATE TRIGGER trg_batch_jobs_updated_at BEFORE UPDATE ON batch_jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE batch_job_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid NOT NULL REFERENCES batch_jobs(id) ON DELETE CASCADE,
    custom_id text NOT NULL,
    request jsonb NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    response jsonb NULL,
    error text NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Workers claim items by job and status; custom_id must be unique per job so
-- results can be matched back to the caller's identifiers.
CREATE INDEX idx_batch_job_items_job_status ON batch_job_items (job_id, status);
CREATE UNIQUE INDEX uq_batch_job_items_job_custom ON batch_job_items (job_id, custom_id);

CREATE TRIGGER trg_batch_job_items_updated_at BEFORE UPDATE ON batch_job_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Transactional outbox: one row per job-level enqueue, written in the same
-- transaction as the job so a crash can't lose the enqueue. Physical Redis
-- delivery is at-least-once; the UNIQUE job_id keeps it one row per job.
CREATE TABLE batch_job_outbox (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid NOT NULL UNIQUE REFERENCES batch_jobs(id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text NULL,
    published_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_batch_job_outbox_updated_at BEFORE UPDATE ON batch_job_outbox
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS batch_job_outbox;
DROP TABLE IF EXISTS batch_job_items;
DROP TABLE IF EXISTS batch_jobs;
