-- +goose Up
-- pgcrypto provides gen_random_uuid(), the default for every primary key.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- set_updated_at keeps updated_at honest on every mutable table so that
-- application code never has to remember to touch it on UPDATE.
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS pgcrypto;
