package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/models"
)

// IdempotencyRepo persists idempotency_records rows. It satisfies
// idempotency.IdempotencyStore: Postgres is the authoritative store; the
// classification semantics come from idempotency.Classify so the repo and
// the in-memory test fake can never drift apart.
type IdempotencyRepo struct {
	pool *pgxpool.Pool
}

// NewIdempotencyRepo returns an IdempotencyRepo backed by pool.
func NewIdempotencyRepo(pool *pgxpool.Pool) *IdempotencyRepo {
	return &IdempotencyRepo{pool: pool}
}

// Lookup reads the record for scope/key and classifies it against
// requestHash via idempotency.Classify. A missing row is OutcomeAbsent, not
// an error.
func (r *IdempotencyRepo) Lookup(ctx context.Context, scope, key, requestHash string) (idempotency.LookupResult, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, scope, idem_key, request_hash, status, locked_until, response_status, response_headers, response_body, created_at, expires_at
		FROM idempotency_records
		WHERE scope = $1 AND idem_key = $2`,
		scope, key)
	rec, err := scanIdempotencyRecord(row)
	if errors.Is(err, ErrNotFound) {
		return idempotency.LookupResult{Outcome: idempotency.OutcomeAbsent}, nil
	}
	if err != nil {
		return idempotency.LookupResult{}, fmt.Errorf("db: lookup idempotency record: %w", err)
	}
	return idempotency.LookupResult{
		Outcome: idempotency.Classify(&rec, requestHash, time.Now()),
		Record:  &rec,
	}, nil
}

// Begin atomically inserts a pending record for scope/key, or reclaims an
// existing one that is expired or a stale pending (lock lapsed). The single
// INSERT … ON CONFLICT … WHERE statement is what makes concurrent same-key
// requests race safely: exactly one wins; the loser gets ErrRecordActive.
// All time comparisons use the database clock (now()), never the app clock,
// so N gateway replicas contend consistently.
func (r *IdempotencyRepo) Begin(ctx context.Context, scope, key, requestHash string, ttl, lockTTL time.Duration) (uuid.UUID, error) {
	// Status literals match the table's CHECK constraint; the models
	// constants mirror them (IdempotencyStatusPending).
	// The reclaim path (ON CONFLICT DO UPDATE) mints a FRESH id
	// (id = gen_random_uuid()). This is load-bearing, not cosmetic: it makes
	// the record identity change on every acquisition, so the previous —
	// possibly still-running or crashed — owner's Complete(oldID) finds no
	// row and safely no-ops instead of overwriting the new owner's in-flight
	// record with a stale response. Keeping the original id (Postgres's
	// default for DO UPDATE) would let a lapsed owner clobber the reclaimer.
	row := r.pool.QueryRow(ctx, `
		INSERT INTO idempotency_records (scope, idem_key, request_hash, status, locked_until, expires_at)
		VALUES ($1, $2, $3, 'pending', now() + make_interval(secs => $4), now() + make_interval(secs => $5))
		ON CONFLICT (scope, idem_key) DO UPDATE SET
			id = gen_random_uuid(),
			request_hash = EXCLUDED.request_hash,
			status = 'pending',
			locked_until = EXCLUDED.locked_until,
			expires_at = EXCLUDED.expires_at,
			response_status = NULL,
			response_headers = NULL,
			response_body = NULL,
			created_at = now()
		WHERE idempotency_records.expires_at <= now()
		   OR (idempotency_records.status = 'pending'
		       AND (idempotency_records.locked_until IS NULL OR idempotency_records.locked_until <= now()))
		RETURNING id`,
		scope, key, requestHash, lockTTL.Seconds(), ttl.Seconds())

	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The conflicting record is live (pending and locked, or completed
			// and unexpired): someone else holds it.
			return uuid.Nil, idempotency.ErrRecordActive
		}
		return uuid.Nil, fmt.Errorf("db: begin idempotency record: %w", err)
	}
	return id, nil
}

// Complete marks a pending record completed with the response to replay.
// It refuses to touch a record that is no longer pending (e.g. reclaimed by
// another request after this one's lock lapsed), reporting ErrNotFound.
func (r *IdempotencyRepo) Complete(ctx context.Context, recordID uuid.UUID, status int, headers, body []byte) error {
	row := r.pool.QueryRow(ctx, `
		UPDATE idempotency_records
		SET status = 'completed',
		    locked_until = NULL,
		    response_status = $2,
		    response_headers = COALESCE($3, '{}'::jsonb),
		    response_body = $4
		WHERE id = $1 AND status = 'pending'
		RETURNING id`,
		recordID, status, normalizeJSONB(headers), normalizeJSONB(body))
	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		return fmt.Errorf("db: complete idempotency record %s: %w", recordID, mapNotFound(err))
	}
	return nil
}

// Release deletes a still-pending record so a same-key retry starts fresh. It
// is the terminal action for retryable (5xx) outcomes: a transient failure
// must not be cached and replayed for the whole TTL, or a client reusing its
// Idempotency-Key (exactly what retry-safety asks of it) would be locked out
// of a working gateway. A record already reclaimed or completed by someone
// else is left untouched (the status/id guard), so Release only ever removes
// the caller's own in-flight record. A missing row is not an error.
func (r *IdempotencyRepo) Release(ctx context.Context, recordID uuid.UUID) error {
	if _, err := r.pool.Exec(ctx,
		`DELETE FROM idempotency_records WHERE id = $1 AND status = 'pending'`,
		recordID); err != nil {
		return fmt.Errorf("db: release idempotency record %s: %w", recordID, err)
	}
	return nil
}

// scanIdempotencyRecord reads one idempotency_records row in the column
// order every query in this file selects.
func scanIdempotencyRecord(row pgx.Row) (models.IdempotencyRecord, error) {
	var rec models.IdempotencyRecord
	if err := row.Scan(&rec.ID, &rec.Scope, &rec.IdemKey, &rec.RequestHash, &rec.Status,
		&rec.LockedUntil, &rec.ResponseStatus, &rec.ResponseHeaders, &rec.ResponseBody,
		&rec.CreatedAt, &rec.ExpiresAt); err != nil {
		return models.IdempotencyRecord{}, mapNotFound(err)
	}
	return rec, nil
}
