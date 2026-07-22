package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
)

// JobRepo persists batch_jobs, batch_job_items, and batch_job_outbox rows.
// It satisfies jobs.JobStore: the transition/aggregation semantics come from
// the pure functions in internal/jobs (encoded here as SQL guards), so this
// repo and the in-memory MemStore cannot drift apart. Absence is reported as
// jobs.ErrNotFound — the sentinel lives with the JobStore contract, mirroring
// how the idempotency repo takes its semantics from internal/idempotency.
type JobRepo struct {
	pool *pgxpool.Pool
}

// NewJobRepo returns a JobRepo backed by pool.
func NewJobRepo(pool *pgxpool.Pool) *JobRepo {
	return &JobRepo{pool: pool}
}

// batchJobColumns is the one column list every batch_jobs query selects, so
// scanBatchJob and the SQL can never disagree on order.
const batchJobColumns = "id, tenant_id, api_key_id, model, status, total_items, completed_items, failed_items, created_at, updated_at"

// batchItemColumns is the shared column list for batch_job_items.
const batchItemColumns = "id, job_id, custom_id, request, status, attempts, response, error, created_at, updated_at"

// outboxColumns is the shared column list for batch_job_outbox.
const outboxColumns = "id, job_id, status, attempts, last_error, published_at, created_at, updated_at"

// CreateWithItemsAndOutbox persists the job, its items, and exactly one
// pending outbox row in a single transaction. Committing them atomically is
// the transactional-outbox guarantee: after commit the enqueue can no longer
// be lost, because either the API publishes the message or the worker's
// outbox drain does.
func (r *JobRepo) CreateWithItemsAndOutbox(ctx context.Context, job models.BatchJob, items []models.BatchJobItem) (models.BatchJob, models.BatchJobOutbox, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return models.BatchJob{}, models.BatchJobOutbox{}, fmt.Errorf("db: begin create batch job: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := scanBatchJob(tx.QueryRow(ctx, `
		INSERT INTO batch_jobs (tenant_id, api_key_id, model, status, total_items)
		VALUES ($1, $2, $3, 'queued', $4)
		RETURNING `+batchJobColumns,
		job.TenantID, job.APIKeyID, job.Model, len(items)))
	if err != nil {
		return models.BatchJob{}, models.BatchJobOutbox{}, fmt.Errorf("db: insert batch job: %w", err)
	}

	for _, it := range items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO batch_job_items (job_id, custom_id, request, status)
			VALUES ($1, $2, $3, 'queued')`,
			stored.ID, it.CustomID, normalizeJSONB(it.Request)); err != nil {
			return models.BatchJob{}, models.BatchJobOutbox{}, fmt.Errorf("db: insert batch job item %q: %w", it.CustomID, err)
		}
	}

	ob, err := scanOutbox(tx.QueryRow(ctx, `
		INSERT INTO batch_job_outbox (job_id, status)
		VALUES ($1, 'pending')
		RETURNING `+outboxColumns,
		stored.ID))
	if err != nil {
		return models.BatchJob{}, models.BatchJobOutbox{}, fmt.Errorf("db: insert batch job outbox: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return models.BatchJob{}, models.BatchJobOutbox{}, fmt.Errorf("db: commit create batch job: %w", err)
	}
	return stored, ob, nil
}

// Get returns the tenant's job. The tenant filter runs in SQL so another
// tenant's job id is indistinguishable from a missing one (jobs.ErrNotFound).
func (r *JobRepo) Get(ctx context.Context, tenantID, jobID uuid.UUID) (models.BatchJob, error) {
	job, err := scanBatchJob(r.pool.QueryRow(ctx, `
		SELECT `+batchJobColumns+`
		FROM batch_jobs
		WHERE id = $1 AND tenant_id = $2`,
		jobID, tenantID))
	if err != nil {
		return models.BatchJob{}, mapJobsNotFound("get batch job", err)
	}
	return job, nil
}

// List returns the tenant's jobs, newest first (id breaks created_at ties so
// pagination-free listings stay stable).
func (r *JobRepo) List(ctx context.Context, tenantID uuid.UUID) ([]models.BatchJob, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+batchJobColumns+`
		FROM batch_jobs
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id DESC`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list batch jobs: %w", err)
	}
	defer rows.Close()

	var out []models.BatchJob
	for rows.Next() {
		job, err := scanBatchJob(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list batch jobs: %w", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list batch jobs: %w", err)
	}
	return out, nil
}

// Items returns the items of the tenant's job ordered by custom_id, or
// jobs.ErrNotFound when the job does not exist for that tenant. custom_id is
// unique within a job, so it is a total, deterministic order; created_at is
// not a usable sort key here because every item of a batch is inserted in one
// transaction and thus shares a created_at, and the uuid id is random.
func (r *JobRepo) Items(ctx context.Context, tenantID, jobID uuid.UUID) ([]models.BatchJobItem, error) {
	// Ownership is checked explicitly (not just via the join) so an empty
	// result can still distinguish "job not yours/missing" from "no items".
	if _, err := r.Get(ctx, tenantID, jobID); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+batchItemColumns+`
		FROM batch_job_items
		WHERE job_id = $1
		ORDER BY custom_id ASC`,
		jobID)
	if err != nil {
		return nil, fmt.Errorf("db: list batch job items: %w", err)
	}
	defer rows.Close()

	var out []models.BatchJobItem
	for rows.Next() {
		it, err := scanBatchItem(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list batch job items: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list batch job items: %w", err)
	}
	return out, nil
}

// MarkJobRunning moves a queued job to running. The status guard makes it
// idempotent across redeliveries and refuses to resurrect a terminal job;
// touching zero rows is therefore success, not an error.
func (r *JobRepo) MarkJobRunning(ctx context.Context, jobID uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE batch_jobs SET status = 'running'
		WHERE id = $1 AND status = 'queued'`,
		jobID); err != nil {
		return fmt.Errorf("db: mark batch job %s running: %w", jobID, err)
	}
	return nil
}

// RequeueRunningItems moves the job's running items back to queued (crash
// recovery on message (re)delivery), preserving attempts, and reports how
// many rows moved.
func (r *JobRepo) RequeueRunningItems(ctx context.Context, jobID uuid.UUID) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE batch_job_items SET status = 'queued'
		WHERE job_id = $1 AND status = 'running'`,
		jobID)
	if err != nil {
		return 0, fmt.Errorf("db: requeue running items for job %s: %w", jobID, err)
	}
	return int(tag.RowsAffected()), nil
}

// ClaimNextQueuedItem atomically claims one queued item of the job inside a
// transaction. FOR UPDATE SKIP LOCKED is what makes two concurrent claimers
// safe: each SELECT locks the row it picked and skips rows locked by the
// other, so the same item can never be handed out twice. An item whose
// attempts are already exhausted is terminally failed here (durably, with an
// explanatory error) instead of being claimed, so it can never wedge the
// queue.
func (r *JobRepo) ClaimNextQueuedItem(ctx context.Context, jobID uuid.UUID, maxAttempts int) (jobs.ClaimResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return jobs.ClaimResult{}, fmt.Errorf("db: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	it, err := scanBatchItem(tx.QueryRow(ctx, `
		SELECT `+batchItemColumns+`
		FROM batch_job_items
		WHERE job_id = $1 AND status = 'queued'
		ORDER BY created_at ASC, id ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
		jobID))
	if errors.Is(err, ErrNotFound) {
		return jobs.ClaimResult{Outcome: jobs.ClaimNone}, nil
	}
	if err != nil {
		return jobs.ClaimResult{}, fmt.Errorf("db: select claimable item: %w", err)
	}

	if it.Attempts+1 > maxAttempts {
		failed, err := scanBatchItem(tx.QueryRow(ctx, `
			UPDATE batch_job_items SET status = 'failed', error = $2
			WHERE id = $1
			RETURNING `+batchItemColumns,
			it.ID, jobs.ExhaustedError(it.Attempts, maxAttempts)))
		if err != nil {
			return jobs.ClaimResult{}, fmt.Errorf("db: exhaust item %s: %w", it.ID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return jobs.ClaimResult{}, fmt.Errorf("db: commit exhaust: %w", err)
		}
		return jobs.ClaimResult{Outcome: jobs.ClaimExhausted, Item: failed}, nil
	}

	claimed, err := scanBatchItem(tx.QueryRow(ctx, `
		UPDATE batch_job_items SET status = 'running', attempts = attempts + 1
		WHERE id = $1
		RETURNING `+batchItemColumns,
		it.ID))
	if err != nil {
		return jobs.ClaimResult{}, fmt.Errorf("db: claim item %s: %w", it.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return jobs.ClaimResult{}, fmt.Errorf("db: commit claim: %w", err)
	}
	return jobs.ClaimResult{Outcome: jobs.ClaimClaimed, Item: claimed}, nil
}

// UpdateItemTerminal writes an item's terminal result. The status = 'running'
// guard is the terminal-immutability rule: only a claimed item can be
// finished, and an item another delivery already finished reports
// jobs.ErrNotFound instead of being overwritten.
func (r *JobRepo) UpdateItemTerminal(ctx context.Context, itemID uuid.UUID, status models.ItemStatus, response json.RawMessage, errMsg *string) error {
	if status != models.ItemStatusSucceeded && status != models.ItemStatusFailed {
		return fmt.Errorf("db: %q is not a terminal item status", status)
	}
	row := r.pool.QueryRow(ctx, `
		UPDATE batch_job_items SET status = $2, response = $3, error = $4
		WHERE id = $1 AND status = 'running'
		RETURNING id`,
		itemID, string(status), normalizeJSONB(response), errMsg)
	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		return mapJobsNotFound(fmt.Sprintf("update item %s terminal", itemID), err)
	}
	return nil
}

// RecomputeAndUpdateJobStatus recounts the job's items, updates the
// denormalized counters, and re-derives the status. The status expression
// mirrors jobs.AggregateJobStatus (the authoritative pure function): any
// non-terminal item leaves the job running; otherwise all-succeeded,
// all-failed, or mixed. It converges under concurrent deliveries because it
// only reads the item table's current truth.
func (r *JobRepo) RecomputeAndUpdateJobStatus(ctx context.Context, jobID uuid.UUID) (models.JobStatus, error) {
	row := r.pool.QueryRow(ctx, `
		WITH counts AS (
			SELECT
				count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
				count(*) FILTER (WHERE status = 'failed') AS failed,
				count(*) AS total
			FROM batch_job_items
			WHERE job_id = $1
		)
		UPDATE batch_jobs j
		SET completed_items = c.succeeded,
		    failed_items = c.failed,
		    status = CASE
		        WHEN c.succeeded + c.failed < c.total THEN 'running'
		        WHEN c.failed = 0 THEN 'succeeded'
		        WHEN c.succeeded = 0 THEN 'failed'
		        ELSE 'partially_failed'
		    END
		FROM counts c
		WHERE j.id = $1
		RETURNING j.status`,
		jobID)
	var status string
	if err := row.Scan(&status); err != nil {
		return "", mapJobsNotFound(fmt.Sprintf("recompute job %s", jobID), err)
	}
	parsed, err := models.ParseJobStatus(status)
	if err != nil {
		return "", fmt.Errorf("db: recompute job %s: %w", jobID, err)
	}
	return parsed, nil
}

// PendingOutbox returns up to limit pending outbox rows, oldest first, for
// the worker's drain loop.
func (r *JobRepo) PendingOutbox(ctx context.Context, limit int) ([]models.BatchJobOutbox, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+outboxColumns+`
		FROM batch_job_outbox
		WHERE status = 'pending'
		ORDER BY created_at ASC, id ASC
		LIMIT $1`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("db: list pending outbox: %w", err)
	}
	defer rows.Close()

	var out []models.BatchJobOutbox
	for rows.Next() {
		ob, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list pending outbox: %w", err)
		}
		out = append(out, ob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list pending outbox: %w", err)
	}
	return out, nil
}

// MarkOutboxPublished durably marks a pending row published. The status
// guard makes duplicate marks (API and drain loop racing) harmless; zero
// rows affected is success.
func (r *JobRepo) MarkOutboxPublished(ctx context.Context, outboxID uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE batch_job_outbox
		SET status = 'published', published_at = now()
		WHERE id = $1 AND status = 'pending'`,
		outboxID); err != nil {
		return fmt.Errorf("db: mark outbox %s published: %w", outboxID, err)
	}
	return nil
}

// MarkOutboxFailedAttempt records one failed publish attempt while leaving
// the row pending, so the drain loop keeps retrying it.
func (r *JobRepo) MarkOutboxFailedAttempt(ctx context.Context, outboxID uuid.UUID, lastErr string) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE batch_job_outbox
		SET attempts = attempts + 1, last_error = $2
		WHERE id = $1 AND status = 'pending'`,
		outboxID, lastErr); err != nil {
		return fmt.Errorf("db: mark outbox %s failed attempt: %w", outboxID, err)
	}
	return nil
}

// mapJobsNotFound wraps repo errors with context and converts driver-level
// no-rows into the JobStore contract's jobs.ErrNotFound (this repo's absence
// sentinel; db.ErrNotFound stays the sentinel for the db-native repos).
func mapJobsNotFound(op string, err error) error {
	if errors.Is(mapNotFound(err), ErrNotFound) {
		return jobs.ErrNotFound
	}
	return fmt.Errorf("db: %s: %w", op, err)
}

// scanBatchJob reads one batch_jobs row in batchJobColumns order, validating
// the status enum so a corrupt row surfaces here.
func scanBatchJob(row pgx.Row) (models.BatchJob, error) {
	var (
		j      models.BatchJob
		status string
	)
	if err := row.Scan(&j.ID, &j.TenantID, &j.APIKeyID, &j.Model, &status, &j.TotalItems,
		&j.CompletedItems, &j.FailedItems, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return models.BatchJob{}, mapNotFound(err)
	}
	parsed, err := models.ParseJobStatus(status)
	if err != nil {
		return models.BatchJob{}, fmt.Errorf("db: batch_jobs row %s: %w", j.ID, err)
	}
	j.Status = parsed
	return j, nil
}

// scanBatchItem reads one batch_job_items row in batchItemColumns order,
// validating the status enum.
func scanBatchItem(row pgx.Row) (models.BatchJobItem, error) {
	var (
		it     models.BatchJobItem
		status string
	)
	if err := row.Scan(&it.ID, &it.JobID, &it.CustomID, &it.Request, &status, &it.Attempts,
		&it.Response, &it.Error, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return models.BatchJobItem{}, mapNotFound(err)
	}
	parsed, err := models.ParseItemStatus(status)
	if err != nil {
		return models.BatchJobItem{}, fmt.Errorf("db: batch_job_items row %s: %w", it.ID, err)
	}
	it.Status = parsed
	return it, nil
}

// scanOutbox reads one batch_job_outbox row in outboxColumns order. The
// outbox status vocabulary is one of the untyped string constants, so it is
// checked against the known set rather than a Parse helper.
func scanOutbox(row pgx.Row) (models.BatchJobOutbox, error) {
	var ob models.BatchJobOutbox
	if err := row.Scan(&ob.ID, &ob.JobID, &ob.Status, &ob.Attempts, &ob.LastError,
		&ob.PublishedAt, &ob.CreatedAt, &ob.UpdatedAt); err != nil {
		return models.BatchJobOutbox{}, mapNotFound(err)
	}
	switch ob.Status {
	case models.OutboxStatusPending, models.OutboxStatusPublished, models.OutboxStatusFailed:
	default:
		return models.BatchJobOutbox{}, fmt.Errorf("db: batch_job_outbox row %s: invalid status %q", ob.ID, ob.Status)
	}
	return ob, nil
}
