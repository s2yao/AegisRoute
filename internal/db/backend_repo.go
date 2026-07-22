package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/models"
)

// BackendRepo persists model_backends rows.
type BackendRepo struct {
	pool *pgxpool.Pool
}

// NewBackendRepo returns a BackendRepo backed by pool.
func NewBackendRepo(pool *pgxpool.Pool) *BackendRepo {
	return &BackendRepo{pool: pool}
}

// Insert creates a backend from b's mutable fields (IDs and timestamps come
// from the database) and returns the full stored row.
func (r *BackendRepo) Insert(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO model_backends (name, base_url, model_name, kind, enabled, priority, weight, max_in_flight)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at`,
		b.Name, b.BaseURL, b.ModelName, string(b.Kind), b.Enabled, b.Priority, b.Weight, b.MaxInFlight)
	return scanBackend(row)
}

// Upsert inserts a backend by name or, when the name already exists, rewrites
// its mutable columns to match b and returns the stored row. It converges the
// row to the supplied desired state, which is exactly what the idempotent
// Stage-3 seeder relies on: re-running seed always leaves the demo backends in
// their declared configuration.
func (r *BackendRepo) Upsert(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO model_backends (name, base_url, model_name, kind, enabled, priority, weight, max_in_flight)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE SET
			base_url = EXCLUDED.base_url,
			model_name = EXCLUDED.model_name,
			kind = EXCLUDED.kind,
			enabled = EXCLUDED.enabled,
			priority = EXCLUDED.priority,
			weight = EXCLUDED.weight,
			max_in_flight = EXCLUDED.max_in_flight
		RETURNING id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at`,
		b.Name, b.BaseURL, b.ModelName, string(b.Kind), b.Enabled, b.Priority, b.Weight, b.MaxInFlight)
	return scanBackend(row)
}

// Update rewrites the mutable columns of the backend identified by b.ID and
// returns the full stored row, or ErrNotFound when no such backend exists.
func (r *BackendRepo) Update(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE model_backends
		SET name = $2, base_url = $3, model_name = $4, kind = $5, enabled = $6, priority = $7, weight = $8, max_in_flight = $9
		WHERE id = $1
		RETURNING id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at`,
		b.ID, b.Name, b.BaseURL, b.ModelName, string(b.Kind), b.Enabled, b.Priority, b.Weight, b.MaxInFlight)
	return scanBackend(row)
}

// GetByID returns the backend with the given id, or ErrNotFound.
func (r *BackendRepo) GetByID(ctx context.Context, id uuid.UUID) (models.ModelBackend, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at
		FROM model_backends
		WHERE id = $1`,
		id)
	return scanBackend(row)
}

// List returns every backend, enabled or not, ordered by (priority, name) so
// admin listings are stable. Unlike ListEnabled it includes soft-disabled
// rows, which is what the control-plane admin API must show.
func (r *BackendRepo) List(ctx context.Context) ([]models.ModelBackend, error) {
	return r.list(ctx, `
		SELECT id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at
		FROM model_backends
		ORDER BY priority ASC, name ASC`)
}

// ListEnabled returns every enabled backend. Ordering by (priority, name) is
// deterministic so the Stage-4 router sees a stable candidate order.
func (r *BackendRepo) ListEnabled(ctx context.Context) ([]models.ModelBackend, error) {
	return r.list(ctx, `
		SELECT id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at
		FROM model_backends
		WHERE enabled
		ORDER BY priority ASC, name ASC`)
}

// ListByModelEnabled returns the enabled backends serving one logical model —
// the router's hot query, covered by idx_model_backends_model_enabled. Same
// deterministic (priority, name) ordering as ListEnabled.
func (r *BackendRepo) ListByModelEnabled(ctx context.Context, modelName string) ([]models.ModelBackend, error) {
	return r.list(ctx, `
		SELECT id, name, base_url, model_name, kind, enabled, priority, weight, max_in_flight, created_at, updated_at
		FROM model_backends
		WHERE model_name = $1 AND enabled
		ORDER BY priority ASC, name ASC`,
		modelName)
}

func (r *BackendRepo) list(ctx context.Context, query string, args ...any) ([]models.ModelBackend, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list model_backends: %w", err)
	}
	defer rows.Close()

	var out []models.ModelBackend
	for rows.Next() {
		b, err := scanBackend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list model_backends: %w", err)
	}
	return out, nil
}

// scanBackend reads one model_backends row in the column order every query
// in this file selects. kind is scanned as a string and validated through
// ParseBackendKind so a corrupt row surfaces as an error here instead of as
// an unroutable request later.
func scanBackend(row pgx.Row) (models.ModelBackend, error) {
	var (
		b    models.ModelBackend
		kind string
	)
	if err := row.Scan(&b.ID, &b.Name, &b.BaseURL, &b.ModelName, &kind, &b.Enabled,
		&b.Priority, &b.Weight, &b.MaxInFlight, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return models.ModelBackend{}, mapNotFound(err)
	}
	k, err := models.ParseBackendKind(kind)
	if err != nil {
		return models.ModelBackend{}, fmt.Errorf("db: model_backends row %s: %w", b.ID, err)
	}
	b.Kind = k
	return b, nil
}
