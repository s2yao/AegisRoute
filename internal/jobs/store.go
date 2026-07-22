package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/models"
)

// ErrNotFound is the absence sentinel for the batch-job store: an unknown
// job id, an id owned by another tenant (indistinguishable on purpose), or a
// terminal-update race that found no matching row. It lives here — not in
// internal/db — because db imports this package for the claim result types,
// mirroring how db imports internal/idempotency for its semantics.
var ErrNotFound = errors.New("jobs: not found")

// ClaimOutcome says what ClaimNextQueuedItem found.
type ClaimOutcome int

const (
	// ClaimNone: no queued item remains for the job — the claim loop is done.
	ClaimNone ClaimOutcome = iota
	// ClaimClaimed: Item was atomically moved queued→running with attempts
	// incremented; the caller must process it and write a terminal result.
	ClaimClaimed
	// ClaimExhausted: Item had already burned all its attempts, so the claim
	// terminally failed it (durably, with an explanatory error) instead of
	// handing it out again. The caller dead-letters it and keeps going — an
	// item must never stay stuck in queued because attempts ran out.
	ClaimExhausted
)

// ClaimResult pairs the outcome with the affected item (zero for ClaimNone).
type ClaimResult struct {
	Outcome ClaimOutcome
	Item    models.BatchJobItem
}

// ExhaustedError renders the terminal error stored on an item whose attempts
// ran out, shared by the Postgres repo and MemStore so the stored text (and
// what tests assert on) cannot drift.
func ExhaustedError(attempts, maxAttempts int) string {
	return fmt.Sprintf("exhausted after %d of %d attempts", attempts, maxAttempts)
}

// JobStore persists batch jobs, their items, and the transactional outbox.
// Postgres is authoritative (satisfied by *db.JobRepo); MemStore is the
// Docker-free in-memory implementation for tests. Both enforce the same
// rules: creation is one atomic transaction, item claims are atomic and
// mutually exclusive, terminal item states are immutable, and job status is
// always derived from item counts via AggregateJobStatus.
//
// Get, List, and Items are tenant-scoped: they only see rows owned by
// tenantID, reporting anything else as ErrNotFound, so a handler cannot leak
// one tenant's jobs to another even if it forgets to filter.
type JobStore interface {
	// CreateWithItemsAndOutbox persists the job (status queued), one row per
	// item (status queued), and exactly one pending outbox row in a single
	// transaction, returning the stored job and outbox row. A crash after
	// commit but before the Redis publish therefore cannot lose the job: the
	// outbox drain re-publishes it.
	CreateWithItemsAndOutbox(ctx context.Context, job models.BatchJob, items []models.BatchJobItem) (models.BatchJob, models.BatchJobOutbox, error)
	// Get returns the tenant's job or ErrNotFound.
	Get(ctx context.Context, tenantID, jobID uuid.UUID) (models.BatchJob, error)
	// List returns the tenant's jobs, newest first.
	List(ctx context.Context, tenantID uuid.UUID) ([]models.BatchJob, error)
	// Items returns the items of the tenant's job in creation order, or
	// ErrNotFound when the job does not exist for that tenant.
	Items(ctx context.Context, tenantID, jobID uuid.UUID) ([]models.BatchJobItem, error)
	// MarkJobRunning moves a queued job to running. It is idempotent across
	// redeliveries: a job already running or terminal is left untouched.
	MarkJobRunning(ctx context.Context, jobID uuid.UUID) error
	// RequeueRunningItems moves the job's running items back to queued,
	// preserving their attempt counts, and reports how many it moved. The
	// worker calls it when a job message is (re)delivered: any item still
	// running at that point was stranded mid-flight by a dead (or reclaimed
	// slow) worker, and requeueing is what lets a later claim retry it —
	// attempts still bound the total retries before the DLQ.
	RequeueRunningItems(ctx context.Context, jobID uuid.UUID) (int, error)
	// ClaimNextQueuedItem atomically claims one queued item of the job:
	// pick a queued item (skipping rows other claimers hold locked), and
	// either terminally fail it when attempts+1 exceeds maxAttempts
	// (ClaimExhausted) or increment attempts, mark it running, and hand it
	// out (ClaimClaimed). Two concurrent claimers can never receive the same
	// item. ClaimNone means no queued items remain.
	ClaimNextQueuedItem(ctx context.Context, jobID uuid.UUID, maxAttempts int) (ClaimResult, error)
	// UpdateItemTerminal writes an item's terminal result (succeeded with a
	// response, or failed with an error). It only touches items currently
	// running — a terminal item is immutable — and reports ErrNotFound when
	// no such running item exists (e.g. another delivery finished it first).
	UpdateItemTerminal(ctx context.Context, itemID uuid.UUID, status models.ItemStatus, response json.RawMessage, errMsg *string) error
	// RecomputeAndUpdateJobStatus recounts the job's items, updates the
	// denormalized counters, derives the status via AggregateJobStatus, and
	// returns it. Safe to call repeatedly and from concurrent deliveries —
	// it converges on the item table's truth.
	RecomputeAndUpdateJobStatus(ctx context.Context, jobID uuid.UUID) (models.JobStatus, error)
	// PendingOutbox returns up to limit unpublished outbox rows, oldest
	// first, for the drain loop.
	PendingOutbox(ctx context.Context, limit int) ([]models.BatchJobOutbox, error)
	// MarkOutboxPublished durably marks a pending outbox row published. Only
	// call after Queue.Publish succeeded. Already-published rows are left
	// untouched.
	MarkOutboxPublished(ctx context.Context, outboxID uuid.UUID) error
	// MarkOutboxFailedAttempt records a failed publish attempt (incrementing
	// attempts, storing the error) while leaving the row pending so the
	// drain loop retries it.
	MarkOutboxFailedAttempt(ctx context.Context, outboxID uuid.UUID, lastErr string) error
}
