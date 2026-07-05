package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/models"
)

// TenantRepo persists tenants rows.
type TenantRepo struct {
	pool *pgxpool.Pool
}

// NewTenantRepo returns a TenantRepo backed by pool.
func NewTenantRepo(pool *pgxpool.Pool) *TenantRepo {
	return &TenantRepo{pool: pool}
}

// Upsert inserts a tenant by name or, when the name already exists, returns
// the existing row. The no-op DO UPDATE (rather than DO NOTHING) makes
// RETURNING always yield the row, which keeps the Stage-3 seeder idempotent.
func (r *TenantRepo) Upsert(ctx context.Context, name string) (models.Tenant, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO tenants (name)
		VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name, created_at, updated_at`,
		name)
	return scanTenant(row)
}

// GetByID returns the tenant with the given id, or ErrNotFound.
func (r *TenantRepo) GetByID(ctx context.Context, id uuid.UUID) (models.Tenant, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, created_at, updated_at
		FROM tenants
		WHERE id = $1`,
		id)
	return scanTenant(row)
}

// scanTenant reads one tenants row in the column order every query in this
// file selects: id, name, created_at, updated_at.
func scanTenant(row pgx.Row) (models.Tenant, error) {
	var t models.Tenant
	if err := row.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return models.Tenant{}, mapNotFound(err)
	}
	return t, nil
}
