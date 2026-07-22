-- +goose Up
CREATE TABLE model_backends (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    base_url text NOT NULL,
    model_name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('openai_compatible', 'mock')),
    enabled boolean NOT NULL DEFAULT true,
    priority integer NOT NULL DEFAULT 0 CHECK (priority >= 0),
    weight integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    max_in_flight integer NOT NULL DEFAULT 1 CHECK (max_in_flight > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The router's hot query is "enabled backends serving this model".
CREATE INDEX idx_model_backends_model_enabled ON model_backends (model_name, enabled);

CREATE TRIGGER trg_model_backends_updated_at BEFORE UPDATE ON model_backends
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE routing_policies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    model_name text NOT NULL,
    strategy text NOT NULL CHECK (strategy IN ('priority_weighted')),
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_routing_policies_updated_at BEFORE UPDATE ON routing_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS routing_policies;
DROP TABLE IF EXISTS model_backends;
