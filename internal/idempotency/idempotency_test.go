package idempotency

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/models"
)

// fakeStore is the in-memory IdempotencyStore, honoring the exact Begin
// reclaim semantics of the Postgres store and classifying through the shared
// Classify. The clock is injectable so lock expiry and TTL expiry are
// driven without sleeping.
type fakeStore struct {
	mu      sync.Mutex
	now     func() time.Time
	records map[string]*models.IdempotencyRecord // key: scope + "\x00" + idemKey

	lookups int
	begins  int
}

func newFakeStore(now func() time.Time) *fakeStore {
	return &fakeStore{now: now, records: map[string]*models.IdempotencyRecord{}}
}

func (f *fakeStore) key(scope, key string) string { return scope + "\x00" + key }

func (f *fakeStore) Lookup(_ context.Context, scope, key, requestHash string) (LookupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	rec := f.records[f.key(scope, key)]
	if rec == nil {
		return LookupResult{Outcome: OutcomeAbsent}, nil
	}
	cp := *rec
	return LookupResult{Outcome: Classify(&cp, requestHash, f.now()), Record: &cp}, nil
}

func (f *fakeStore) Begin(_ context.Context, scope, key, requestHash string, ttl, lockTTL time.Duration) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	now := f.now()
	k := f.key(scope, key)
	if rec := f.records[k]; rec != nil {
		expired := !now.Before(rec.ExpiresAt)
		stale := rec.Status == models.IdempotencyStatusPending &&
			(rec.LockedUntil == nil || !now.Before(*rec.LockedUntil))
		if !expired && !stale {
			return uuid.Nil, ErrRecordActive
		}
	}
	locked := now.Add(lockTTL)
	f.records[k] = &models.IdempotencyRecord{
		ID:          uuid.New(),
		Scope:       scope,
		IdemKey:     key,
		RequestHash: requestHash,
		Status:      models.IdempotencyStatusPending,
		LockedUntil: &locked,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	return f.records[k].ID, nil
}

func (f *fakeStore) Complete(_ context.Context, recordID uuid.UUID, status int, headers, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.records {
		if rec.ID == recordID && rec.Status == models.IdempotencyStatusPending {
			rec.Status = models.IdempotencyStatusCompleted
			rec.LockedUntil = nil
			rec.ResponseStatus = &status
			rec.ResponseHeaders = headers
			rec.ResponseBody = body
			return nil
		}
	}
	return ErrRecordActive // no pending record with that id
}

func (f *fakeStore) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, rec := range f.records {
		if rec.Status == models.IdempotencyStatusPending {
			n++
		}
	}
	return n
}

// testClock is a manually advanced time source.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	testTTL     = time.Hour
	testLockTTL = time.Minute
	testScope   = "tenant:t1:key:k1:POST:/v1/chat/completions"
	hashA       = "hash-a"
	hashB       = "hash-b"
)

func newTestCoordinator() (*Coordinator, *fakeStore, *testClock) {
	clock := newTestClock()
	store := newFakeStore(clock.now)
	return NewCoordinator(store, testTTL, testLockTTL), store, clock
}

// --- Classify: the semantic core ---------------------------------------------

func TestClassify(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	pendingLocked := func(hash string) *models.IdempotencyRecord {
		l := future
		return &models.IdempotencyRecord{RequestHash: hash, Status: models.IdempotencyStatusPending,
			LockedUntil: &l, ExpiresAt: future}
	}

	t.Run("nil record is absent", func(t *testing.T) {
		assert.Equal(t, OutcomeAbsent, Classify(nil, hashA, now))
	})
	t.Run("expired record is absent even with matching body", func(t *testing.T) {
		rec := &models.IdempotencyRecord{RequestHash: hashA,
			Status: models.IdempotencyStatusCompleted, ExpiresAt: past}
		assert.Equal(t, OutcomeAbsent, Classify(rec, hashA, now))
	})
	t.Run("expired record is absent even with different body", func(t *testing.T) {
		rec := &models.IdempotencyRecord{RequestHash: hashB,
			Status: models.IdempotencyStatusCompleted, ExpiresAt: past}
		assert.Equal(t, OutcomeAbsent, Classify(rec, hashA, now))
	})
	t.Run("different body conflicts regardless of state", func(t *testing.T) {
		completed := &models.IdempotencyRecord{RequestHash: hashB,
			Status: models.IdempotencyStatusCompleted, ExpiresAt: future}
		assert.Equal(t, OutcomeConflictBody, Classify(completed, hashA, now))
		assert.Equal(t, OutcomeConflictBody, Classify(pendingLocked(hashB), hashA, now))
	})
	t.Run("completed same body replays", func(t *testing.T) {
		rec := &models.IdempotencyRecord{RequestHash: hashA,
			Status: models.IdempotencyStatusCompleted, ExpiresAt: future}
		assert.Equal(t, OutcomeReplay, Classify(rec, hashA, now))
	})
	t.Run("pending same body with live lock is in progress", func(t *testing.T) {
		assert.Equal(t, OutcomeInProgress, Classify(pendingLocked(hashA), hashA, now))
	})
	t.Run("pending with expired lock is stale", func(t *testing.T) {
		l := past
		rec := &models.IdempotencyRecord{RequestHash: hashA,
			Status: models.IdempotencyStatusPending, LockedUntil: &l, ExpiresAt: future}
		assert.Equal(t, OutcomeStale, Classify(rec, hashA, now))
	})
	t.Run("pending without a lock is stale (defensive)", func(t *testing.T) {
		rec := &models.IdempotencyRecord{RequestHash: hashA,
			Status: models.IdempotencyStatusPending, ExpiresAt: future}
		assert.Equal(t, OutcomeStale, Classify(rec, hashA, now))
	})
}

// --- Coordinator flow ----------------------------------------------------------

func TestCoordinatorBypassWithoutKey(t *testing.T) {
	c, store, _ := newTestCoordinator()
	ctx := context.Background()

	dec, err := c.Check(ctx, testScope, "", hashA)
	require.NoError(t, err)
	assert.Equal(t, ActionProceed, dec.Action)

	dec, err = c.Begin(ctx, testScope, "", hashA)
	require.NoError(t, err)
	assert.Equal(t, ActionProceed, dec.Action)

	assert.Zero(t, store.lookups, "no key → the store is never touched")
	assert.Zero(t, store.begins)
}

func TestCoordinatorFullLifecycle(t *testing.T) {
	c, store, _ := newTestCoordinator()
	ctx := context.Background()

	// New key: Check proceeds, Begin opens a pending record.
	dec, err := c.Check(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionProceed, dec.Action)

	dec, err = c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionStarted, dec.Action)
	require.NotEqual(t, uuid.Nil, dec.RecordID)
	assert.Equal(t, 1, store.pendingCount())

	// Complete it, then the same key+body replays.
	require.NoError(t, c.Complete(ctx, dec.RecordID, 200,
		map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`)))
	assert.Zero(t, store.pendingCount())

	replay, err := c.Check(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionReplay, replay.Action)
	require.NotNil(t, replay.Stored)
	assert.Equal(t, 200, replay.Stored.Status)
	assert.Equal(t, "application/json", replay.Stored.Headers["Content-Type"])
	assert.JSONEq(t, `{"ok":true}`, string(replay.Stored.Body))
}

func TestCoordinatorConflictOnDifferentBody(t *testing.T) {
	c, _, _ := newTestCoordinator()
	ctx := context.Background()

	dec, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionStarted, dec.Action)

	// Different body against the pending record: conflict at Check.
	conflict, err := c.Check(ctx, testScope, "key-1", hashB)
	require.NoError(t, err)
	assert.Equal(t, ActionConflictBody, conflict.Action)

	// Still a conflict after completion.
	require.NoError(t, c.Complete(ctx, dec.RecordID, 200, nil, []byte(`{}`)))
	conflict, err = c.Check(ctx, testScope, "key-1", hashB)
	require.NoError(t, err)
	assert.Equal(t, ActionConflictBody, conflict.Action)
}

func TestCoordinatorInProgressWhilePending(t *testing.T) {
	c, _, _ := newTestCoordinator()
	ctx := context.Background()

	first, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionStarted, first.Action)

	// Same key+body while the first is pending: in-progress at Check…
	dec, err := c.Check(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	assert.Equal(t, ActionInProgress, dec.Action)

	// …and Begin loses the race the same way.
	dec, err = c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	assert.Equal(t, ActionInProgress, dec.Action)
}

func TestCoordinatorReclaimsStalePending(t *testing.T) {
	c, store, clock := newTestCoordinator()
	ctx := context.Background()

	first, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	firstID := first.RecordID

	// The lock expires (owner died mid-flight): the next Begin reclaims.
	clock.advance(testLockTTL + time.Second)
	second, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionStarted, second.Action)
	assert.NotEqual(t, firstID, second.RecordID, "reclaim issues a fresh record id")
	assert.Equal(t, 1, store.pendingCount(), "reclaim replaces, never duplicates")

	// The dead owner's late Complete must not corrupt the reclaimed record.
	err = c.Complete(ctx, firstID, 200, nil, []byte(`{}`))
	assert.Error(t, err, "completing a reclaimed (superseded) record fails")
}

func TestCoordinatorExpiredRecordTreatedAsNew(t *testing.T) {
	c, _, clock := newTestCoordinator()
	ctx := context.Background()

	dec, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.NoError(t, c.Complete(ctx, dec.RecordID, 200, nil, []byte(`{"v":1}`)))

	clock.advance(testTTL + time.Second)

	// Past the TTL the completed record no longer replays…
	check, err := c.Check(ctx, testScope, "key-1", hashB)
	require.NoError(t, err)
	assert.Equal(t, ActionProceed, check.Action, "expired records are treated as absent, even for a different body")

	// …and Begin reclaims the row for the new request.
	again, err := c.Begin(ctx, testScope, "key-1", hashB)
	require.NoError(t, err)
	assert.Equal(t, ActionStarted, again.Action)
}

func TestCoordinatorBeginRaceFoldsToReplay(t *testing.T) {
	c, _, _ := newTestCoordinator()
	ctx := context.Background()

	// Someone else completed the record between our Check and Begin.
	other, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.NoError(t, c.Complete(ctx, other.RecordID, 201, nil, []byte(`{"winner":true}`)))

	dec, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.Equal(t, ActionReplay, dec.Action, "a lost Begin race against a completed record replays it")
	assert.Equal(t, 201, dec.Stored.Status)
}

func TestCoordinatorCompleteStripsRequestID(t *testing.T) {
	c, store, _ := newTestCoordinator()
	ctx := context.Background()

	dec, err := c.Begin(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	require.NoError(t, c.Complete(ctx, dec.RecordID, 200, map[string]string{
		"Content-Type": "application/json",
		"X-Request-ID": "should-never-be-stored",
	}, []byte(`{}`)))

	rec := store.records[store.key(testScope, "key-1")]
	var stored map[string]string
	require.NoError(t, json.Unmarshal(rec.ResponseHeaders, &stored))
	assert.NotContains(t, stored, "X-Request-ID",
		"X-Request-ID is never stored; every replay uses the current request's id")
	assert.Contains(t, stored, "Content-Type")

	replay, err := c.Check(ctx, testScope, "key-1", hashA)
	require.NoError(t, err)
	assert.NotContains(t, replay.Stored.Headers, "X-Request-ID")
}

func TestScope(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := Scope(tenant, key, "POST", "/v1/chat/completions")
	assert.Equal(t,
		"tenant:11111111-1111-1111-1111-111111111111:key:22222222-2222-2222-2222-222222222222:POST:/v1/chat/completions",
		got, "matches the format documented on the idempotency_records migration")

	other := Scope(tenant, key, "POST", "/api/v1/batch-jobs")
	assert.NotEqual(t, got, other, "route is part of the scope: keys never collide across routes")
}
