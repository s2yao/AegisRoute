package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/models"
)

// InferenceRequestRepo persists inference_requests rows — the append-only
// ledger of every completion the gateway served or failed to serve.
type InferenceRequestRepo struct {
	pool *pgxpool.Pool
}

// NewInferenceRequestRepo returns an InferenceRequestRepo backed by pool.
func NewInferenceRequestRepo(pool *pgxpool.Pool) *InferenceRequestRepo {
	return &InferenceRequestRepo{pool: pool}
}

// Insert appends one request record (ID and created_at come from the
// database) and returns the full stored row. BackendID and CacheResult are
// nullable: a request can fail before a backend is chosen, and cache results
// only exist from Stage 5 on.
func (r *InferenceRequestRepo) Insert(ctx context.Context, req models.InferenceRequest) (models.InferenceRequest, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO inference_requests (tenant_id, api_key_id, model, backend_id, cache_result, status, latency_ms, request_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, tenant_id, api_key_id, model, backend_id, cache_result, status, latency_ms, request_hash, created_at`,
		req.TenantID, req.APIKeyID, req.Model, req.BackendID, req.CacheResult,
		req.Status, req.LatencyMS, req.RequestHash)
	return scanInferenceRequest(row)
}

// scanInferenceRequest reads one inference_requests row in the column order
// Insert returns.
func scanInferenceRequest(row pgx.Row) (models.InferenceRequest, error) {
	var out models.InferenceRequest
	if err := row.Scan(&out.ID, &out.TenantID, &out.APIKeyID, &out.Model, &out.BackendID,
		&out.CacheResult, &out.Status, &out.LatencyMS, &out.RequestHash, &out.CreatedAt); err != nil {
		return models.InferenceRequest{}, mapNotFound(err)
	}
	return out, nil
}
