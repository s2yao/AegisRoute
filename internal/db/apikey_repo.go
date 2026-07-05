package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/models"
)

// APIKeyRepo persists api_keys rows. Only HMAC hashes of raw keys ever reach
// this repo; the raw credential never touches the database.
type APIKeyRepo struct {
	pool *pgxpool.Pool
}

// NewAPIKeyRepo returns an APIKeyRepo backed by pool.
func NewAPIKeyRepo(pool *pgxpool.Pool) *APIKeyRepo {
	return &APIKeyRepo{pool: pool}
}

// Upsert inserts an API key or, when key_hash already exists, refreshes its
// display name and returns the existing row — so the Stage-3 seeder can rerun
// without minting duplicate credentials.
func (r *APIKeyRepo) Upsert(ctx context.Context, tenantID uuid.UUID, name, keyHash string) (models.APIKey, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO api_keys (tenant_id, name, key_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (key_hash) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, tenant_id, name, key_hash, created_at, updated_at`,
		tenantID, name, keyHash)
	return scanAPIKey(row)
}

// GetByHash resolves a presented key's hash to its row, or ErrNotFound. This
// is the auth hot path: one indexed lookup on the UNIQUE key_hash column.
func (r *APIKeyRepo) GetByHash(ctx context.Context, keyHash string) (models.APIKey, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, key_hash, created_at, updated_at
		FROM api_keys
		WHERE key_hash = $1`,
		keyHash)
	return scanAPIKey(row)
}

// scanAPIKey reads one api_keys row in the column order every query in this
// file selects: id, tenant_id, name, key_hash, created_at, updated_at.
func scanAPIKey(row pgx.Row) (models.APIKey, error) {
	var k models.APIKey
	if err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.KeyHash, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return models.APIKey{}, mapNotFound(err)
	}
	return k, nil
}
