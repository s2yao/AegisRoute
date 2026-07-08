package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/models"
)

// MemStore is the in-memory JobStore used by Docker-free tests (worker pool
// and batch-handler tests). It enforces the same semantics as the Postgres
// repo — atomic claims, immutable terminal items, tenant-scoped reads,
// status via the shared pure functions — under one mutex, so concurrent
// claimers contend exactly like FOR UPDATE SKIP LOCKED serializes them.
type MemStore struct {
	mu     sync.Mutex
	jobs   map[uuid.UUID]*models.BatchJob
	items  map[uuid.UUID][]*models.BatchJobItem // by job id, creation order
	outbox map[uuid.UUID]*models.BatchJobOutbox // by outbox id
}

// NewMemStore returns an empty in-memory job store.
func NewMemStore() *MemStore {
	return &MemStore{
		jobs:   map[uuid.UUID]*models.BatchJob{},
		items:  map[uuid.UUID][]*models.BatchJobItem{},
		outbox: map[uuid.UUID]*models.BatchJobOutbox{},
	}
}

// CreateWithItemsAndOutbox stores the job, its items, and one pending outbox
// row atomically (one lock hold = one transaction).
func (s *MemStore) CreateWithItemsAndOutbox(_ context.Context, job models.BatchJob, items []models.BatchJobItem) (models.BatchJob, models.BatchJobOutbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	job.ID = uuid.New()
	job.Status = models.JobStatusQueued
	job.TotalItems = len(items)
	job.CompletedItems = 0
	job.FailedItems = 0
	job.CreatedAt = now
	job.UpdatedAt = now
	s.jobs[job.ID] = &job

	stored := make([]*models.BatchJobItem, 0, len(items))
	for _, it := range items {
		it.ID = uuid.New()
		it.JobID = job.ID
		it.Status = models.ItemStatusQueued
		it.Attempts = 0
		it.CreatedAt = now
		it.UpdatedAt = now
		copied := it
		stored = append(stored, &copied)
	}
	s.items[job.ID] = stored

	ob := models.BatchJobOutbox{
		ID:        uuid.New(),
		JobID:     job.ID,
		Status:    models.OutboxStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.outbox[ob.ID] = &ob

	return job, ob, nil
}

// Get returns the tenant's job or ErrNotFound.
func (s *MemStore) Get(_ context.Context, tenantID, jobID uuid.UUID) (models.BatchJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.TenantID != tenantID {
		return models.BatchJob{}, ErrNotFound
	}
	return *job, nil
}

// List returns the tenant's jobs, newest first.
func (s *MemStore) List(_ context.Context, tenantID uuid.UUID) ([]models.BatchJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.BatchJob, 0)
	for _, job := range s.jobs {
		if job.TenantID == tenantID {
			out = append(out, *job)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// Items returns the tenant's job's items in creation order, or ErrNotFound.
func (s *MemStore) Items(_ context.Context, tenantID, jobID uuid.UUID) ([]models.BatchJobItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.TenantID != tenantID {
		return nil, ErrNotFound
	}
	stored := s.items[jobID]
	out := make([]models.BatchJobItem, 0, len(stored))
	for _, it := range stored {
		out = append(out, *it)
	}
	// Mirror JobRepo.Items: return items in a deterministic order by custom_id
	// (unique within a job), so fake-backed tests observe the same ordering
	// contract as the Postgres store.
	sort.Slice(out, func(i, j int) bool { return out[i].CustomID < out[j].CustomID })
	return out, nil
}

// MarkJobRunning moves a queued job to running; any other state is a no-op.
func (s *MemStore) MarkJobRunning(_ context.Context, jobID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return ErrNotFound
	}
	if ValidJobTransition(job.Status, models.JobStatusRunning) {
		job.Status = models.JobStatusRunning
		job.UpdatedAt = time.Now()
	}
	return nil
}

// RequeueRunningItems moves the job's running items back to queued,
// preserving attempts.
func (s *MemStore) RequeueRunningItems(_ context.Context, jobID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	moved := 0
	for _, it := range s.items[jobID] {
		if it.Status == models.ItemStatusRunning {
			it.Status = models.ItemStatusQueued
			it.UpdatedAt = time.Now()
			moved++
		}
	}
	return moved, nil
}

// ClaimNextQueuedItem atomically claims (or exhausts) the oldest queued item.
// The single mutex plays the role of FOR UPDATE SKIP LOCKED: two claimers
// serialize here and can never receive the same item.
func (s *MemStore) ClaimNextQueuedItem(_ context.Context, jobID uuid.UUID, maxAttempts int) (ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items[jobID] {
		if it.Status != models.ItemStatusQueued {
			continue
		}
		if it.Attempts+1 > maxAttempts {
			msg := ExhaustedError(it.Attempts, maxAttempts)
			it.Status = models.ItemStatusFailed
			it.Error = &msg
			it.UpdatedAt = time.Now()
			return ClaimResult{Outcome: ClaimExhausted, Item: *it}, nil
		}
		it.Attempts++
		it.Status = models.ItemStatusRunning
		it.UpdatedAt = time.Now()
		return ClaimResult{Outcome: ClaimClaimed, Item: *it}, nil
	}
	return ClaimResult{Outcome: ClaimNone}, nil
}

// UpdateItemTerminal writes a terminal result onto a running item; terminal
// items are immutable (ErrNotFound), mirroring the repo's status guard.
func (s *MemStore) UpdateItemTerminal(_ context.Context, itemID uuid.UUID, status models.ItemStatus, response json.RawMessage, errMsg *string) error {
	if status != models.ItemStatusSucceeded && status != models.ItemStatusFailed {
		return fmt.Errorf("jobs: %q is not a terminal item status", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, its := range s.items {
		for _, it := range its {
			if it.ID != itemID {
				continue
			}
			if !ValidItemTransition(it.Status, status) {
				return ErrNotFound
			}
			it.Status = status
			it.Response = append(json.RawMessage(nil), response...)
			it.Error = errMsg
			it.UpdatedAt = time.Now()
			return nil
		}
	}
	return ErrNotFound
}

// RecomputeAndUpdateJobStatus recounts items and re-derives the job status.
func (s *MemStore) RecomputeAndUpdateJobStatus(_ context.Context, jobID uuid.UUID) (models.JobStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return "", ErrNotFound
	}
	succeeded, failed := 0, 0
	for _, it := range s.items[jobID] {
		switch it.Status {
		case models.ItemStatusSucceeded:
			succeeded++
		case models.ItemStatusFailed:
			failed++
		}
	}
	job.CompletedItems = succeeded
	job.FailedItems = failed
	job.Status = AggregateJobStatus(job.TotalItems, succeeded, failed)
	job.UpdatedAt = time.Now()
	return job.Status, nil
}

// PendingOutbox returns up to limit pending outbox rows, oldest first.
func (s *MemStore) PendingOutbox(_ context.Context, limit int) ([]models.BatchJobOutbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.BatchJobOutbox, 0)
	for _, ob := range s.outbox {
		if ob.Status == models.OutboxStatusPending {
			out = append(out, *ob)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkOutboxPublished marks a pending row published; other states are no-ops.
func (s *MemStore) MarkOutboxPublished(_ context.Context, outboxID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ob, ok := s.outbox[outboxID]
	if !ok {
		return ErrNotFound
	}
	if ob.Status == models.OutboxStatusPending {
		now := time.Now()
		ob.Status = models.OutboxStatusPublished
		ob.PublishedAt = &now
		ob.UpdatedAt = now
	}
	return nil
}

// MarkOutboxFailedAttempt records a failed publish, leaving the row pending.
func (s *MemStore) MarkOutboxFailedAttempt(_ context.Context, outboxID uuid.UUID, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ob, ok := s.outbox[outboxID]
	if !ok {
		return ErrNotFound
	}
	if ob.Status == models.OutboxStatusPending {
		ob.Attempts++
		ob.LastError = &lastErr
		ob.UpdatedAt = time.Now()
	}
	return nil
}

// Outbox returns the outbox row for a job (test helper), or ErrNotFound.
func (s *MemStore) Outbox(jobID uuid.UUID) (models.BatchJobOutbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ob := range s.outbox {
		if ob.JobID == jobID {
			return *ob, nil
		}
	}
	return models.BatchJobOutbox{}, ErrNotFound
}
