//go:build integration

package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
)

// stage2Tables lists every table the Stage-2 migrations create, in an order
// safe to truncate with CASCADE. The suite truncates them all up front so
// reruns against a persistent database stay deterministic.
var stage2Tables = []string{
	"idempotency_records",
	"backend_health_snapshots",
	"batch_job_outbox",
	"batch_job_items",
	"batch_jobs",
	"inference_requests",
	"routing_policies",
	"model_backends",
	"api_keys",
	"tenants",
}

// TestIntegration exercises migrations and every repository against the real
// Postgres named by DATABASE_URL. No test or subtest in this file uses
// t.Parallel(): goose.SetBaseFS mutates process-global goose state.
func TestIntegration(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping db integration tests (run via 'make test-integration')")
	}

	ctx := context.Background()

	// Running migrations twice proves idempotency: the second run must be a
	// clean no-op, exactly what 'gateway-api -migrate' relies on at deploy.
	require.NoError(t, RunMigrations(ctx, databaseURL))
	require.NoError(t, RunMigrations(ctx, databaseURL), "second RunMigrations must be a no-op")

	pool, err := Connect(ctx, &config.Config{DatabaseURL: databaseURL})
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, Ping(ctx, pool))

	for _, table := range stage2Tables {
		_, err := pool.Exec(ctx, "TRUNCATE "+table+" CASCADE")
		require.NoErrorf(t, err, "truncate %s", table)
	}

	t.Run("TenantRepo", func(t *testing.T) {
		repo := NewTenantRepo(pool)

		first, err := repo.Upsert(ctx, "acme")
		require.NoError(t, err)
		assert.Equal(t, "acme", first.Name)
		assert.NotEqual(t, uuid.Nil, first.ID)

		second, err := repo.Upsert(ctx, "acme")
		require.NoError(t, err)
		assert.Equal(t, first.ID, second.ID, "upsert with the same name must return the same row")

		got, err := repo.GetByID(ctx, first.ID)
		require.NoError(t, err)
		assert.Equal(t, first.ID, got.ID)
		assert.Equal(t, "acme", got.Name)

		_, err = repo.GetByID(ctx, uuid.New())
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("APIKeyRepo", func(t *testing.T) {
		tenant, err := NewTenantRepo(pool).Upsert(ctx, "acme-keys")
		require.NoError(t, err)

		repo := NewAPIKeyRepo(pool)

		created, err := repo.Upsert(ctx, tenant.ID, "primary", "hash-1")
		require.NoError(t, err)
		assert.Equal(t, tenant.ID, created.TenantID)
		assert.Equal(t, "primary", created.Name)
		assert.Equal(t, "hash-1", created.KeyHash)

		got, err := repo.GetByHash(ctx, "hash-1")
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)

		renamed, err := repo.Upsert(ctx, tenant.ID, "rotated", "hash-1")
		require.NoError(t, err)
		assert.Equal(t, created.ID, renamed.ID, "same key_hash must keep the same row")
		assert.Equal(t, "rotated", renamed.Name)

		_, err = repo.GetByHash(ctx, "nope")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("BackendRepo", func(t *testing.T) {
		repo := NewBackendRepo(pool)

		fast, err := repo.Insert(ctx, models.ModelBackend{
			Name:        "mock-fast",
			BaseURL:     "http://mock-fast:8081",
			ModelName:   "llama-fast",
			Kind:        models.BackendKindMock,
			Enabled:     true,
			Priority:    2,
			Weight:      2,
			MaxInFlight: 4,
		})
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, fast.ID)
		assert.Equal(t, models.BackendKindMock, fast.Kind)

		byModel, err := repo.ListByModelEnabled(ctx, "llama-fast")
		require.NoError(t, err)
		require.Len(t, byModel, 1)
		assert.Equal(t, fast.ID, byModel[0].ID)

		fast.Priority = 3
		fast.Weight = 5
		updated, err := repo.Update(ctx, fast)
		require.NoError(t, err)
		assert.Equal(t, 3, updated.Priority)
		assert.Equal(t, 5, updated.Weight)

		got, err := repo.GetByID(ctx, fast.ID)
		require.NoError(t, err)
		assert.Equal(t, 3, got.Priority)
		assert.Equal(t, 5, got.Weight)
		assert.False(t, got.UpdatedAt.Before(got.CreatedAt),
			"the set_updated_at trigger must keep updated_at >= created_at")

		_, err = repo.GetByID(ctx, uuid.New())
		assert.ErrorIs(t, err, ErrNotFound, "GetByID on a missing id must report ErrNotFound")

		cheap, err := repo.Insert(ctx, models.ModelBackend{
			Name:        "mock-cheap",
			BaseURL:     "http://mock-cheap:8082",
			ModelName:   "llama-fast",
			Kind:        models.BackendKindMock,
			Enabled:     true,
			Priority:    1,
			Weight:      1,
			MaxInFlight: 2,
		})
		require.NoError(t, err)

		enabled, err := repo.ListEnabled(ctx)
		require.NoError(t, err)
		require.Len(t, enabled, 2)
		assert.Equal(t, cheap.ID, enabled[0].ID, "priority 1 must sort before priority 3")
		assert.Equal(t, fast.ID, enabled[1].ID)

		got.Enabled = false
		disabled, err := repo.Update(ctx, got)
		require.NoError(t, err)
		assert.False(t, disabled.Enabled)

		enabled, err = repo.ListEnabled(ctx)
		require.NoError(t, err)
		require.Len(t, enabled, 1, "soft-disabled backend must not be listed")
		assert.Equal(t, cheap.ID, enabled[0].ID)

		byModel, err = repo.ListByModelEnabled(ctx, "llama-fast")
		require.NoError(t, err)
		require.Len(t, byModel, 1, "soft-disabled backend must not be routable")
		assert.Equal(t, cheap.ID, byModel[0].ID)

		_, err = repo.Update(ctx, models.ModelBackend{
			ID:          uuid.New(),
			Name:        "ghost",
			BaseURL:     "http://ghost:1",
			ModelName:   "llama-fast",
			Kind:        models.BackendKindMock,
			Priority:    0,
			Weight:      1,
			MaxInFlight: 1,
		})
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("RoutingPolicyRepo", func(t *testing.T) {
		repo := NewRoutingPolicyRepo(pool)

		policy, err := repo.Insert(ctx, models.RoutingPolicy{
			Name:      "default-llama-fast",
			ModelName: "llama-fast",
			Strategy:  models.StrategyPriorityWeighted,
			Config:    nil, // nil must land as '{}' via the COALESCE default
			Enabled:   true,
		})
		require.NoError(t, err)
		assert.JSONEq(t, "{}", string(policy.Config), "nil Config must be stored as the '{}' default")

		list, err := repo.List(ctx)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, policy.ID, list[0].ID)
		assert.JSONEq(t, "{}", string(list[0].Config))

		got, err := repo.GetForModel(ctx, "llama-fast")
		require.NoError(t, err)
		assert.Equal(t, policy.ID, got.ID)
		assert.Equal(t, models.StrategyPriorityWeighted, got.Strategy)

		got.Enabled = false
		updated, err := repo.Update(ctx, got)
		require.NoError(t, err)
		assert.False(t, updated.Enabled)

		_, err = repo.GetForModel(ctx, "llama-fast")
		assert.ErrorIs(t, err, ErrNotFound)

		// A non-nil but contentless Config must also land as the '{}' default
		// rather than raise a JSON syntax error from Postgres (regression for
		// the COALESCE-only-guards-NULL footgun).
		contentless := []struct {
			name   string
			config json.RawMessage
		}{
			{"empty-config", json.RawMessage{}},
			{"whitespace-config", json.RawMessage("   ")},
		}
		for _, tc := range contentless {
			p, err := repo.Insert(ctx, models.RoutingPolicy{
				Name:      tc.name,
				ModelName: "llama-empty",
				Strategy:  models.StrategyPriorityWeighted,
				Config:    tc.config,
				Enabled:   true,
			})
			require.NoErrorf(t, err, "insert %s", tc.name)
			assert.JSONEqf(t, "{}", string(p.Config),
				"%s: contentless Config must default to '{}'", tc.name)
		}

		_, err = repo.Update(ctx, models.RoutingPolicy{
			ID:        uuid.New(),
			Name:      "ghost-policy",
			ModelName: "llama-fast",
			Strategy:  models.StrategyPriorityWeighted,
			Enabled:   true,
		})
		assert.ErrorIs(t, err, ErrNotFound, "Update on a missing id must report ErrNotFound")
	})

	t.Run("InferenceRequestRepo", func(t *testing.T) {
		tenant, err := NewTenantRepo(pool).Upsert(ctx, "acme-ledger")
		require.NoError(t, err)
		key, err := NewAPIKeyRepo(pool).Upsert(ctx, tenant.ID, "ledger-key", "hash-ledger")
		require.NoError(t, err)
		backend, err := NewBackendRepo(pool).Insert(ctx, models.ModelBackend{
			Name:        "ledger-backend",
			BaseURL:     "http://ledger-backend:8081",
			ModelName:   "llama-fast",
			Kind:        models.BackendKindMock,
			Enabled:     true,
			Priority:    1,
			Weight:      1,
			MaxInFlight: 4,
		})
		require.NoError(t, err)

		repo := NewInferenceRequestRepo(pool)

		row, err := repo.Insert(ctx, models.InferenceRequest{
			TenantID:    tenant.ID,
			APIKeyID:    key.ID,
			Model:       "llama-fast",
			BackendID:   &backend.ID,
			Status:      models.RequestStatusSucceeded,
			LatencyMS:   42,
			RequestHash: "deadbeef",
		})
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, row.ID)
		assert.Equal(t, tenant.ID, row.TenantID)
		assert.Equal(t, key.ID, row.APIKeyID)
		require.NotNil(t, row.BackendID)
		assert.Equal(t, backend.ID, *row.BackendID)
		assert.Nil(t, row.CacheResult, "no cache result before Stage 5")
		assert.Equal(t, models.RequestStatusSucceeded, row.Status)
		assert.Equal(t, 42, row.LatencyMS)
		assert.False(t, row.CreatedAt.IsZero())

		// BackendID is nullable: a request can fail before routing chose one.
		failed, err := repo.Insert(ctx, models.InferenceRequest{
			TenantID:    tenant.ID,
			APIKeyID:    key.ID,
			Model:       "llama-fast",
			Status:      models.RequestStatusFailed,
			LatencyMS:   0,
			RequestHash: "cafef00d",
		})
		require.NoError(t, err)
		assert.Nil(t, failed.BackendID)

		_, err = repo.Insert(ctx, models.InferenceRequest{
			TenantID:    tenant.ID,
			APIKeyID:    key.ID,
			Model:       "llama-fast",
			Status:      "exploded",
			LatencyMS:   1,
			RequestHash: "bad",
		})
		assert.Error(t, err, "the status CHECK constraint rejects unknown values")
	})

	t.Run("IdempotencyRepo", func(t *testing.T) {
		repo := NewIdempotencyRepo(pool)
		const (
			scope = "tenant:t1:key:k1:POST:/v1/chat/completions"
			hashA = "hash-a"
			hashB = "hash-b"
		)
		ttl, lockTTL := time.Hour, time.Minute

		// Absent → Begin inserts a pending record.
		res, err := repo.Lookup(ctx, scope, "key-1", hashA)
		require.NoError(t, err)
		assert.Equal(t, idempotency.OutcomeAbsent, res.Outcome)

		firstID, err := repo.Begin(ctx, scope, "key-1", hashA, ttl, lockTTL)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, firstID)

		// A live pending record: same body in-progress, different body conflict,
		// and a concurrent Begin loses with ErrRecordActive.
		res, err = repo.Lookup(ctx, scope, "key-1", hashA)
		require.NoError(t, err)
		assert.Equal(t, idempotency.OutcomeInProgress, res.Outcome)
		res, err = repo.Lookup(ctx, scope, "key-1", hashB)
		require.NoError(t, err)
		assert.Equal(t, idempotency.OutcomeConflictBody, res.Outcome)
		_, err = repo.Begin(ctx, scope, "key-1", hashA, ttl, lockTTL)
		assert.ErrorIs(t, err, idempotency.ErrRecordActive,
			"a live pending record must not be reclaimed")

		// Complete → replay carries the stored response.
		require.NoError(t, repo.Complete(ctx, firstID, 200,
			[]byte(`{"Content-Type":"application/json"}`), []byte(`{"ok":true}`)))
		res, err = repo.Lookup(ctx, scope, "key-1", hashA)
		require.NoError(t, err)
		require.Equal(t, idempotency.OutcomeReplay, res.Outcome)
		require.NotNil(t, res.Record.ResponseStatus)
		assert.Equal(t, 200, *res.Record.ResponseStatus)
		assert.JSONEq(t, `{"ok":true}`, string(res.Record.ResponseBody))

		// A completed record is held: Begin refuses, Complete refuses twice.
		_, err = repo.Begin(ctx, scope, "key-1", hashA, ttl, lockTTL)
		assert.ErrorIs(t, err, idempotency.ErrRecordActive)
		err = repo.Complete(ctx, firstID, 500, nil, []byte(`{}`))
		assert.ErrorIs(t, err, ErrNotFound, "completing a non-pending record must fail")

		// Stale pending (lockTTL=0 → immediately reclaimable): Begin reclaims
		// atomically and issues a fresh id; the dead owner's Complete fails.
		staleID, err := repo.Begin(ctx, scope, "key-stale", hashA, ttl, 0)
		require.NoError(t, err)
		reclaimedID, err := repo.Begin(ctx, scope, "key-stale", hashB, ttl, lockTTL)
		require.NoError(t, err, "a stale pending record is reclaimed")
		assert.NotEqual(t, staleID, reclaimedID)
		err = repo.Complete(ctx, staleID, 200, nil, []byte(`{}`))
		assert.ErrorIs(t, err, ErrNotFound, "the superseded owner cannot complete")

		// Expired record (ttl=0): treated as absent and reclaimed by Begin.
		expiredID, err := repo.Begin(ctx, scope, "key-expired", hashA, 0, lockTTL)
		require.NoError(t, err)
		require.NoError(t, repo.Complete(ctx, expiredID, 200, nil, []byte(`{}`)))
		res, err = repo.Lookup(ctx, scope, "key-expired", hashA)
		require.NoError(t, err)
		assert.Equal(t, idempotency.OutcomeAbsent, res.Outcome, "expired records are absent")
		_, err = repo.Begin(ctx, scope, "key-expired", hashA, ttl, lockTTL)
		assert.NoError(t, err, "an expired record is reclaimed by Begin")

		// Scopes are isolated: the same key in another scope is independent.
		_, err = repo.Begin(ctx, "tenant:t2:key:k2:POST:/v1/chat/completions", "key-1", hashA, ttl, lockTTL)
		assert.NoError(t, err)

		// Release deletes a pending record so the key is free to retry; a
		// released key Begins fresh, and releasing a superseded id is a no-op.
		relID, err := repo.Begin(ctx, scope, "key-release", hashA, ttl, lockTTL)
		require.NoError(t, err)
		require.NoError(t, repo.Release(ctx, relID))
		res, err = repo.Lookup(ctx, scope, "key-release", hashA)
		require.NoError(t, err)
		assert.Equal(t, idempotency.OutcomeAbsent, res.Outcome, "a released record is gone")
		_, err = repo.Begin(ctx, scope, "key-release", hashB, ttl, lockTTL)
		assert.NoError(t, err, "a released key Begins fresh")
		assert.NoError(t, repo.Release(ctx, relID), "releasing an already-gone id is a no-op")
	})

	t.Run("JobRepo", func(t *testing.T) {
		tenant, err := NewTenantRepo(pool).Upsert(ctx, "acme-batch")
		require.NoError(t, err)
		other, err := NewTenantRepo(pool).Upsert(ctx, "other-batch")
		require.NoError(t, err)
		key, err := NewAPIKeyRepo(pool).Upsert(ctx, tenant.ID, "batch-key", "hash-batch")
		require.NoError(t, err)

		repo := NewJobRepo(pool)

		// CreateWithItemsAndOutbox persists the job (queued), its items
		// (queued), and one pending outbox row in one transaction.
		job, outbox, err := repo.CreateWithItemsAndOutbox(ctx, models.BatchJob{
			TenantID: tenant.ID,
			APIKeyID: key.ID,
			Model:    "llama-fast",
		}, []models.BatchJobItem{
			{CustomID: "a", Request: json.RawMessage(`{"model":"llama-fast"}`)},
			{CustomID: "b", Request: json.RawMessage(`{"model":"llama-fast"}`)},
		})
		require.NoError(t, err)
		assert.Equal(t, models.JobStatusQueued, job.Status)
		assert.Equal(t, 2, job.TotalItems)
		assert.Equal(t, models.OutboxStatusPending, outbox.Status)
		assert.Equal(t, job.ID, outbox.JobID)

		// Tenant scoping: the owner reads it, another tenant gets ErrNotFound.
		got, err := repo.Get(ctx, tenant.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, job.ID, got.ID)
		_, err = repo.Get(ctx, other.ID, job.ID)
		assert.ErrorIs(t, err, jobs.ErrNotFound, "another tenant must not read the job")
		_, err = repo.Items(ctx, other.ID, job.ID)
		assert.ErrorIs(t, err, jobs.ErrNotFound)

		list, err := repo.List(ctx, tenant.ID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, job.ID, list[0].ID)
		assert.Empty(t, mustList(t, repo, other.ID), "another tenant's list is empty")

		items, err := repo.Items(ctx, tenant.ID, job.ID)
		require.NoError(t, err)
		require.Len(t, items, 2)
		// Items come back in a deterministic custom_id order: both share a
		// created_at (inserted in one transaction), so the order is stable
		// across runs, not dependent on the random uuid id.
		assert.Equal(t, "a", items[0].CustomID)
		assert.Equal(t, "b", items[1].CustomID)

		// MarkJobRunning is idempotent: queued→running once, then a no-op.
		require.NoError(t, repo.MarkJobRunning(ctx, job.ID))
		got, err = repo.Get(ctx, tenant.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, models.JobStatusRunning, got.Status)
		require.NoError(t, repo.MarkJobRunning(ctx, job.ID), "second MarkJobRunning is a no-op")

		// Claim both items; each claim is atomic and increments attempts.
		claim1, err := repo.ClaimNextQueuedItem(ctx, job.ID, 3)
		require.NoError(t, err)
		require.Equal(t, jobs.ClaimClaimed, claim1.Outcome)
		assert.Equal(t, 1, claim1.Item.Attempts)
		assert.Equal(t, models.ItemStatusRunning, claim1.Item.Status)

		claim2, err := repo.ClaimNextQueuedItem(ctx, job.ID, 3)
		require.NoError(t, err)
		require.Equal(t, jobs.ClaimClaimed, claim2.Outcome)
		assert.NotEqual(t, claim1.Item.ID, claim2.Item.ID, "two claims never return the same item")

		// No queued items remain.
		none, err := repo.ClaimNextQueuedItem(ctx, job.ID, 3)
		require.NoError(t, err)
		assert.Equal(t, jobs.ClaimNone, none.Outcome)

		// Terminal write succeeds once; a second write finds no running row.
		require.NoError(t, repo.UpdateItemTerminal(ctx, claim1.Item.ID,
			models.ItemStatusSucceeded, json.RawMessage(`{"ok":true}`), nil))
		err = repo.UpdateItemTerminal(ctx, claim1.Item.ID, models.ItemStatusFailed, nil, strPtr("no overwrite"))
		assert.ErrorIs(t, err, jobs.ErrNotFound, "a terminal item is immutable")

		// One success, one failure → partially_failed with the counters set.
		require.NoError(t, repo.UpdateItemTerminal(ctx, claim2.Item.ID,
			models.ItemStatusFailed, nil, strPtr("boom")))
		status, err := repo.RecomputeAndUpdateJobStatus(ctx, job.ID)
		require.NoError(t, err)
		assert.Equal(t, models.JobStatusPartiallyFailed, status)
		got, err = repo.Get(ctx, tenant.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, got.CompletedItems)
		assert.Equal(t, 1, got.FailedItems)

		// Outbox: pending → failed attempt stays pending → published clears it.
		pending, err := repo.PendingOutbox(ctx, 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, outbox.ID, pending[0].ID)

		require.NoError(t, repo.MarkOutboxFailedAttempt(ctx, outbox.ID, "redis down"))
		pending, err = repo.PendingOutbox(ctx, 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, 1, pending[0].Attempts)
		require.NotNil(t, pending[0].LastError)

		require.NoError(t, repo.MarkOutboxPublished(ctx, outbox.ID))
		pending, err = repo.PendingOutbox(ctx, 10)
		require.NoError(t, err)
		assert.Empty(t, pending, "a published row leaves the pending set")
	})

	t.Run("JobRepoClaimExhaustion", func(t *testing.T) {
		tenant, err := NewTenantRepo(pool).Upsert(ctx, "acme-exhaust")
		require.NoError(t, err)
		key, err := NewAPIKeyRepo(pool).Upsert(ctx, tenant.ID, "exhaust-key", "hash-exhaust")
		require.NoError(t, err)
		repo := NewJobRepo(pool)

		job, _, err := repo.CreateWithItemsAndOutbox(ctx, models.BatchJob{
			TenantID: tenant.ID, APIKeyID: key.ID, Model: "llama-fast",
		}, []models.BatchJobItem{{CustomID: "solo", Request: json.RawMessage(`{"model":"llama-fast"}`)}})
		require.NoError(t, err)

		// maxAttempts=1: claim (attempts→1), requeue (crash recovery), then
		// the next claim would need attempts=2>1 so it exhausts the item
		// terminally instead of leaving it stuck queued.
		c, err := repo.ClaimNextQueuedItem(ctx, job.ID, 1)
		require.NoError(t, err)
		require.Equal(t, jobs.ClaimClaimed, c.Outcome)
		_, err = repo.RequeueRunningItems(ctx, job.ID)
		require.NoError(t, err)

		ex, err := repo.ClaimNextQueuedItem(ctx, job.ID, 1)
		require.NoError(t, err)
		require.Equal(t, jobs.ClaimExhausted, ex.Outcome)
		assert.Equal(t, models.ItemStatusFailed, ex.Item.Status)
		require.NotNil(t, ex.Item.Error)
		assert.Contains(t, *ex.Item.Error, "exhausted")

		// The exhausted item is terminal, so nothing remains claimable.
		none, err := repo.ClaimNextQueuedItem(ctx, job.ID, 1)
		require.NoError(t, err)
		assert.Equal(t, jobs.ClaimNone, none.Outcome)
	})
}

// mustList is a small helper for the JobRepo subtest's negative-space
// assertion.
func mustList(t *testing.T, repo *JobRepo, tenantID uuid.UUID) []models.BatchJob {
	t.Helper()
	list, err := repo.List(context.Background(), tenantID)
	require.NoError(t, err)
	return list
}

func strPtr(s string) *string { return &s }
