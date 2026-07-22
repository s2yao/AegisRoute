package jobs_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
)

func newItems(n int) []models.BatchJobItem {
	out := make([]models.BatchJobItem, n)
	for i := 0; i < n; i++ {
		out[i] = models.BatchJobItem{
			CustomID: "req-" + uuid.NewString(),
			Request:  json.RawMessage(`{"model":"llama-fast"}`),
		}
	}
	return out
}

func TestMemStore_CreateWithItemsAndOutbox(t *testing.T) {
	s := jobs.NewMemStore()
	tenant := uuid.New()

	job, ob, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "llama-fast"}, newItems(3))
	require.NoError(t, err)

	assert.Equal(t, models.JobStatusQueued, job.Status)
	assert.Equal(t, 3, job.TotalItems)
	assert.Equal(t, 0, job.CompletedItems)
	assert.Equal(t, models.OutboxStatusPending, ob.Status)
	assert.Equal(t, job.ID, ob.JobID)

	items, err := s.Items(context.Background(), tenant, job.ID)
	require.NoError(t, err)
	require.Len(t, items, 3)
	for _, it := range items {
		assert.Equal(t, models.ItemStatusQueued, it.Status)
		assert.Equal(t, 0, it.Attempts)
	}
}

func TestMemStore_TenantScoping(t *testing.T) {
	s := jobs.NewMemStore()
	owner, other := uuid.New(), uuid.New()
	job, _, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: owner, APIKeyID: uuid.New(), Model: "m"}, newItems(1))
	require.NoError(t, err)

	// The owner sees the job; another tenant gets ErrNotFound, never the row.
	_, err = s.Get(context.Background(), owner, job.ID)
	require.NoError(t, err)
	_, err = s.Get(context.Background(), other, job.ID)
	assert.ErrorIs(t, err, jobs.ErrNotFound)

	_, err = s.Items(context.Background(), other, job.ID)
	assert.ErrorIs(t, err, jobs.ErrNotFound)

	list, err := s.List(context.Background(), other)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestMemStore_ClaimExhaustsWhenAttemptsExceeded(t *testing.T) {
	s := jobs.NewMemStore()
	tenant := uuid.New()
	job, _, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "m"}, newItems(1))
	require.NoError(t, err)

	// maxAttempts=1: the first claim increments attempts to 1 and hands the
	// item out; requeue it (crash recovery) and the next claim would need
	// attempts=2 > 1, so it exhausts instead of looping forever in queued.
	res, err := s.ClaimNextQueuedItem(context.Background(), job.ID, 1)
	require.NoError(t, err)
	require.Equal(t, jobs.ClaimClaimed, res.Outcome)
	assert.Equal(t, 1, res.Item.Attempts)

	_, err = s.RequeueRunningItems(context.Background(), job.ID)
	require.NoError(t, err)

	res, err = s.ClaimNextQueuedItem(context.Background(), job.ID, 1)
	require.NoError(t, err)
	require.Equal(t, jobs.ClaimExhausted, res.Outcome)
	assert.Equal(t, models.ItemStatusFailed, res.Item.Status)
	require.NotNil(t, res.Item.Error)
	assert.Contains(t, *res.Item.Error, "exhausted")

	// No queued item remains — the item is terminally failed, not stuck.
	res, err = s.ClaimNextQueuedItem(context.Background(), job.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, jobs.ClaimNone, res.Outcome)
}

func TestMemStore_TerminalItemsAreImmutable(t *testing.T) {
	s := jobs.NewMemStore()
	tenant := uuid.New()
	job, _, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "m"}, newItems(1))
	require.NoError(t, err)

	res, err := s.ClaimNextQueuedItem(context.Background(), job.ID, 3)
	require.NoError(t, err)
	require.Equal(t, jobs.ClaimClaimed, res.Outcome)

	require.NoError(t, s.UpdateItemTerminal(context.Background(), res.Item.ID,
		models.ItemStatusSucceeded, json.RawMessage(`{"ok":true}`), nil))

	// A second terminal write for the same item finds no running row: the
	// stored result is authoritative (this is what makes redelivery safe).
	err = s.UpdateItemTerminal(context.Background(), res.Item.ID,
		models.ItemStatusFailed, nil, strPtr("should not overwrite"))
	assert.ErrorIs(t, err, jobs.ErrNotFound)
}

func TestMemStore_RecomputeAndUpdateJobStatus(t *testing.T) {
	tests := []struct {
		name       string
		outcomes   []models.ItemStatus // terminal status to write per item
		wantStatus models.JobStatus
		wantDone   int
		wantFailed int
	}{
		{"all succeeded", []models.ItemStatus{models.ItemStatusSucceeded, models.ItemStatusSucceeded}, models.JobStatusSucceeded, 2, 0},
		{"all failed", []models.ItemStatus{models.ItemStatusFailed, models.ItemStatusFailed}, models.JobStatusFailed, 0, 2},
		{"mixed", []models.ItemStatus{models.ItemStatusSucceeded, models.ItemStatusFailed}, models.JobStatusPartiallyFailed, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := jobs.NewMemStore()
			tenant := uuid.New()
			job, _, err := s.CreateWithItemsAndOutbox(context.Background(),
				models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "m"}, newItems(len(tt.outcomes)))
			require.NoError(t, err)

			for _, want := range tt.outcomes {
				res, err := s.ClaimNextQueuedItem(context.Background(), job.ID, 3)
				require.NoError(t, err)
				require.Equal(t, jobs.ClaimClaimed, res.Outcome)
				var resp json.RawMessage
				var errMsg *string
				if want == models.ItemStatusSucceeded {
					resp = json.RawMessage(`{"ok":true}`)
				} else {
					errMsg = strPtr("boom")
				}
				require.NoError(t, s.UpdateItemTerminal(context.Background(), res.Item.ID, want, resp, errMsg))
			}

			status, err := s.RecomputeAndUpdateJobStatus(context.Background(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)

			got, err := s.Get(context.Background(), tenant, job.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantDone, got.CompletedItems)
			assert.Equal(t, tt.wantFailed, got.FailedItems)
		})
	}
}

func TestMemStore_ConcurrentClaimsNeverDuplicate(t *testing.T) {
	// Two simulated workers hammering the same job must partition the items:
	// the atomic claim (FOR UPDATE SKIP LOCKED in Postgres, one mutex here)
	// guarantees no item is handed out twice.
	s := jobs.NewMemStore()
	tenant := uuid.New()
	const n = 200
	job, _, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "m"}, newItems(n))
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]int{}
	)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				res, err := s.ClaimNextQueuedItem(context.Background(), job.ID, 3)
				require.NoError(t, err)
				if res.Outcome == jobs.ClaimNone {
					return
				}
				mu.Lock()
				claimed[res.Item.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, claimed, n, "every item claimed exactly once")
	for id, count := range claimed {
		assert.Equalf(t, 1, count, "item %s claimed %d times", id, count)
	}
}

func TestMemStore_OutboxDrainLifecycle(t *testing.T) {
	s := jobs.NewMemStore()
	tenant := uuid.New()
	job, ob, err := s.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: tenant, APIKeyID: uuid.New(), Model: "m"}, newItems(1))
	require.NoError(t, err)

	pending, err := s.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, job.ID, pending[0].JobID)

	// A failed attempt keeps the row pending (still drained next tick).
	require.NoError(t, s.MarkOutboxFailedAttempt(context.Background(), ob.ID, "redis down"))
	pending, err = s.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, 1, pending[0].Attempts)

	// A successful publish marks it published and removes it from the drain set.
	require.NoError(t, s.MarkOutboxPublished(context.Background(), ob.ID))
	pending, err = s.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func strPtr(s string) *string { return &s }
