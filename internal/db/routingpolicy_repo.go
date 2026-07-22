package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/models"
)

// RoutingPolicyRepo persists routing_policies rows.
type RoutingPolicyRepo struct {
	pool *pgxpool.Pool
}

// NewRoutingPolicyRepo returns a RoutingPolicyRepo backed by pool.
func NewRoutingPolicyRepo(pool *pgxpool.Pool) *RoutingPolicyRepo {
	return &RoutingPolicyRepo{pool: pool}
}

// Insert creates a policy and returns the full stored row. A contentless
// Config (nil, empty, or whitespace-only) is normalized to SQL NULL so the
// COALESCE default lands the column's NOT NULL as '{}'; non-empty Config must
// be valid JSON or Postgres rejects it, which is the caller's bug to fix.
func (r *RoutingPolicyRepo) Insert(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO routing_policies (name, model_name, strategy, config, enabled)
		VALUES ($1, $2, $3, COALESCE($4, '{}'::jsonb), $5)
		RETURNING id, name, model_name, strategy, config, enabled, created_at, updated_at`,
		p.Name, p.ModelName, p.Strategy, normalizeJSONB(p.Config), p.Enabled)
	return scanRoutingPolicy(row)
}

// Upsert inserts a policy by name or, when the name already exists, rewrites
// its mutable columns to match p and returns the stored row. Config gets the
// same contentless-to-'{}' normalization as Insert. It converges the row to
// the supplied desired state so the idempotent Stage-3 seeder can re-run
// safely.
func (r *RoutingPolicyRepo) Upsert(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO routing_policies (name, model_name, strategy, config, enabled)
		VALUES ($1, $2, $3, COALESCE($4, '{}'::jsonb), $5)
		ON CONFLICT (name) DO UPDATE SET
			model_name = EXCLUDED.model_name,
			strategy = EXCLUDED.strategy,
			config = EXCLUDED.config,
			enabled = EXCLUDED.enabled
		RETURNING id, name, model_name, strategy, config, enabled, created_at, updated_at`,
		p.Name, p.ModelName, p.Strategy, normalizeJSONB(p.Config), p.Enabled)
	return scanRoutingPolicy(row)
}

// GetByID returns the policy with the given id, or ErrNotFound. Used by the
// admin PATCH handler to load a policy before applying its mutable fields.
func (r *RoutingPolicyRepo) GetByID(ctx context.Context, id uuid.UUID) (models.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, model_name, strategy, config, enabled, created_at, updated_at
		FROM routing_policies
		WHERE id = $1`,
		id)
	return scanRoutingPolicy(row)
}

// Update rewrites the mutable columns of the policy identified by p.ID and
// returns the full stored row, or ErrNotFound when no such policy exists.
// Config gets the same contentless-to-'{}' normalization as Insert.
func (r *RoutingPolicyRepo) Update(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE routing_policies
		SET name = $2, model_name = $3, strategy = $4, config = COALESCE($5, '{}'::jsonb), enabled = $6
		WHERE id = $1
		RETURNING id, name, model_name, strategy, config, enabled, created_at, updated_at`,
		p.ID, p.Name, p.ModelName, p.Strategy, normalizeJSONB(p.Config), p.Enabled)
	return scanRoutingPolicy(row)
}

// List returns every policy, enabled or not, ordered by name for stable
// admin listings.
func (r *RoutingPolicyRepo) List(ctx context.Context) ([]models.RoutingPolicy, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, model_name, strategy, config, enabled, created_at, updated_at
		FROM routing_policies
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: list routing_policies: %w", err)
	}
	defer rows.Close()

	var out []models.RoutingPolicy
	for rows.Next() {
		p, err := scanRoutingPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list routing_policies: %w", err)
	}
	return out, nil
}

// GetForModel returns the enabled policy for one logical model, or
// ErrNotFound when none exists. Oldest-first ordering makes the pick
// deterministic if several enabled policies ever name the same model.
func (r *RoutingPolicyRepo) GetForModel(ctx context.Context, modelName string) (models.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, model_name, strategy, config, enabled, created_at, updated_at
		FROM routing_policies
		WHERE model_name = $1 AND enabled
		ORDER BY created_at ASC
		LIMIT 1`,
		modelName)
	return scanRoutingPolicy(row)
}

// scanRoutingPolicy reads one routing_policies row in the column order every
// query in this file selects: id, name, model_name, strategy, config,
// enabled, created_at, updated_at.
func scanRoutingPolicy(row pgx.Row) (models.RoutingPolicy, error) {
	var p models.RoutingPolicy
	if err := row.Scan(&p.ID, &p.Name, &p.ModelName, &p.Strategy, &p.Config,
		&p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return models.RoutingPolicy{}, mapNotFound(err)
	}
	return p, nil
}

// normalizeJSONB collapses a nil, empty, or whitespace-only RawMessage to nil
// so the COALESCE default in Insert/Update can supply '{}'. Without it a
// non-nil but contentless RawMessage binds as an argument Postgres tries to
// parse as JSON and rejects (SQLSTATE 22P02) before COALESCE ever runs. It
// intentionally does not validate genuine content: malformed JSON is a caller
// error that should surface, not be silently swallowed.
func normalizeJSONB(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return raw
}
