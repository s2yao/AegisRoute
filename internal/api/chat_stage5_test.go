package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/cache"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/ratelimit"
	"github.com/example/aegisroute/internal/routing"
)

// fakeIdemStore is the api-side in-memory idempotency.IdempotencyStore,
// mirroring the Postgres store's Begin/reclaim semantics via the shared
// Classify. maxPending proves concurrent same-key requests never hold two
// pending records at once.
type fakeIdemStore struct {
	mu         sync.Mutex
	records    map[string]*models.IdempotencyRecord
	maxPending int
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{records: map[string]*models.IdempotencyRecord{}}
}

func (f *fakeIdemStore) storeKey(scope, key string) string { return scope + "\x00" + key }

func (f *fakeIdemStore) Lookup(_ context.Context, scope, key, requestHash string) (idempotency.LookupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.records[f.storeKey(scope, key)]
	if rec == nil {
		return idempotency.LookupResult{Outcome: idempotency.OutcomeAbsent}, nil
	}
	cp := *rec
	return idempotency.LookupResult{
		Outcome: idempotency.Classify(&cp, requestHash, time.Now()),
		Record:  &cp,
	}, nil
}

func (f *fakeIdemStore) Begin(_ context.Context, scope, key, requestHash string, ttl, lockTTL time.Duration) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	k := f.storeKey(scope, key)
	if rec := f.records[k]; rec != nil {
		expired := !now.Before(rec.ExpiresAt)
		stale := rec.Status == models.IdempotencyStatusPending &&
			(rec.LockedUntil == nil || !now.Before(*rec.LockedUntil))
		if !expired && !stale {
			return uuid.Nil, idempotency.ErrRecordActive
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
	if n := f.pendingLocked(); n > f.maxPending {
		f.maxPending = n
	}
	return f.records[k].ID, nil
}

func (f *fakeIdemStore) Complete(_ context.Context, recordID uuid.UUID, status int, headers, body []byte) error {
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
	return idempotency.ErrRecordActive
}

func (f *fakeIdemStore) Release(_ context.Context, recordID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, rec := range f.records {
		if rec.ID == recordID && rec.Status == models.IdempotencyStatusPending {
			delete(f.records, k)
			return nil
		}
	}
	return nil
}

func (f *fakeIdemStore) pendingLocked() int {
	n := 0
	for _, rec := range f.records {
		if rec.Status == models.IdempotencyStatusPending {
			n++
		}
	}
	return n
}

func (f *fakeIdemStore) counts() (pending, completed, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.records {
		switch rec.Status {
		case models.IdempotencyStatusPending:
			pending++
		case models.IdempotencyStatusCompleted:
			completed++
		}
	}
	return pending, completed, len(f.records)
}

// stage5Fixture wires the REAL cache, rate limiter (both over miniredis),
// and idempotency Coordinator (over the in-memory store) into the chat
// fixture's fake selector/inference.
type stage5Fixture struct {
	*chatFixture
	store    *fakeIdemStore
	redis    *miniredis.Miniredis
	backends int64 // atomic count of backend calls
}

func newStage5Fixture(t *testing.T, qps int) *stage5Fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	f := &stage5Fixture{chatFixture: newChatFixture(t), store: newFakeIdemStore(), redis: mr}
	f.deps.Cache = cache.New(rdb, time.Minute)
	f.deps.Limiter = ratelimit.New(rdb, qps, time.Second)
	f.deps.Idempotency = idempotency.NewCoordinator(f.store, time.Hour, time.Minute)
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		atomic.AddInt64(&f.backends, 1)
		return &inference.Response{StatusCode: http.StatusOK, Body: []byte(upstreamJSON)}, nil
	})
	f.rebuild()
	return f
}

func (f *stage5Fixture) backendCalls() int64 { return atomic.LoadInt64(&f.backends) }

// postChatKeyed sends an authenticated chat request with an Idempotency-Key
// (empty = none) and optional explicit X-Request-ID.
func (f *stage5Fixture) postChatKeyed(t *testing.T, body, idemKey, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	return do(t, f.handler, req)
}

// cacheableBody has temperature 0 — the E2E cache demo shape — so it is
// cache-eligible; validChatBody (no temperature) is not.
const cacheableBody = `{"model":"llama-fast","temperature":0,"messages":[{"role":"user","content":"hello"}]}`

// --- the E2E-shaped sequence: MISS → HIT → 429 --------------------------------

func TestStage5MissThenHitThenRateLimit(t *testing.T) {
	f := newStage5Fixture(t, 2)

	// First call: cache MISS, backend called, ledger row carries the backend.
	rec := f.postChatKeyed(t, cacheableBody, "idem-key-1", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "MISS", rec.Header().Get("X-AegisRoute-Cache"))
	assert.Equal(t, "mock-llm-fast", rec.Header().Get("X-AegisRoute-Backend"))
	assert.Equal(t, int64(1), f.backendCalls())

	// Second call: identical body, DIFFERENT Idempotency-Key — idempotency
	// does not replay, so this is a genuine cache HIT with no backend call.
	rec = f.postChatKeyed(t, cacheableBody, "idem-key-2", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "HIT", rec.Header().Get("X-AegisRoute-Cache"))
	assert.JSONEq(t, upstreamJSON, rec.Body.String(), "the cached body is replayed verbatim")
	assert.Empty(t, rec.Header().Get("X-AegisRoute-Backend"), "no backend served a cache hit")
	assert.Equal(t, int64(1), f.backendCalls(), "a HIT must not call a backend")

	// Ledger: miss row with backend id, hit row with null backend.
	rows := f.ledger.all()
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].CacheResult)
	assert.Equal(t, models.CacheResultMiss, *rows[0].CacheResult)
	assert.NotNil(t, rows[0].BackendID)
	require.NotNil(t, rows[1].CacheResult)
	assert.Equal(t, models.CacheResultHit, *rows[1].CacheResult)
	assert.Nil(t, rows[1].BackendID, "on HIT backend_id is null: no backend was called")

	// Both idempotency records completed even though the second was a HIT.
	pending, completed, _ := f.store.counts()
	assert.Zero(t, pending, "a cache HIT still completes its opened idempotency record")
	assert.Equal(t, 2, completed)

	// Third call in the same window: over the QPS limit → 429, and no pending
	// record is created (the limit runs before Begin).
	rec = f.postChatKeyed(t, cacheableBody, "idem-key-3", "")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "rate_limited", decodeError(t, rec.Body).Error.Code)
	pending, completed, total := f.store.counts()
	assert.Zero(t, pending)
	assert.Equal(t, 2, completed)
	assert.Equal(t, 2, total, "a rate-limited request never creates a record")
}

// --- idempotent replay ---------------------------------------------------------

func TestStage5IdempotentReplay(t *testing.T) {
	f := newStage5Fixture(t, 100)

	first := f.postChatKeyed(t, validChatBody, "replay-key", "")
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, int64(1), f.backendCalls())

	// Same key + same body: replayed from the record — before rate limiting,
	// without touching a backend, and with the CURRENT request's id.
	second := f.postChatKeyed(t, validChatBody, "replay-key", "request-id-two")
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String(), "the stored response body is replayed")
	assert.Equal(t, "mock-llm-fast", second.Header().Get("X-AegisRoute-Backend"),
		"safe stored headers are replayed")
	assert.Equal(t, "request-id-two", second.Header().Get("X-Request-ID"),
		"a replay carries the current request's X-Request-ID, never the original's")
	assert.Equal(t, int64(1), f.backendCalls(), "a replay must not call a backend")
	assert.Len(t, f.ledger.all(), 1, "a replay is not a new inference; no second ledger row")
}

func TestStage5ReplaySkipsRateLimit(t *testing.T) {
	f := newStage5Fixture(t, 1)

	first := f.postChatKeyed(t, validChatBody, "replay-key", "")
	require.Equal(t, http.StatusOK, first.Code)

	// The window's single slot is spent, but a completed replay is free: the
	// idempotency lookup runs before the rate limiter.
	second := f.postChatKeyed(t, validChatBody, "replay-key", "")
	assert.Equal(t, http.StatusOK, second.Code,
		"completed idempotency replay happens before rate limiting")

	// New work in the same window is still limited.
	third := f.postChatKeyed(t, validChatBody, "fresh-key", "")
	assert.Equal(t, http.StatusTooManyRequests, third.Code)
}

// --- conflicts ------------------------------------------------------------------

func TestStage5ConflictOnChangedBody(t *testing.T) {
	f := newStage5Fixture(t, 100)

	first := f.postChatKeyed(t, validChatBody, "conflict-key", "")
	require.Equal(t, http.StatusOK, first.Code)

	changed := `{"model":"llama-fast","messages":[{"role":"user","content":"DIFFERENT"}]}`
	rec := f.postChatKeyed(t, changed, "conflict-key", "")
	assert.Equal(t, http.StatusConflict, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "conflict", env.Error.Code)
	assert.Equal(t, int64(1), f.backendCalls(), "a body conflict never reaches a backend")
}

func TestStage5ConcurrentSameKeyOnePendingRecord(t *testing.T) {
	f := newStage5Fixture(t, 100)

	// Gate the backend so the first request holds its pending record while
	// the second arrives.
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		entered <- struct{}{}
		<-gate
		return &inference.Response{StatusCode: http.StatusOK, Body: []byte(upstreamJSON)}, nil
	})
	f.rebuild()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- f.postChatKeyed(t, validChatBody, "race-key", "") }()

	select {
	case <-entered: // first request is inside the backend, record pending
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the backend")
	}

	second := f.postChatKeyed(t, validChatBody, "race-key", "")
	assert.Equal(t, http.StatusConflict, second.Code)
	env := decodeError(t, second.Body)
	assert.Equal(t, "conflict", env.Error.Code)
	assert.Contains(t, env.Error.Message, "already in progress")

	close(gate)
	first := <-firstDone
	assert.Equal(t, http.StatusOK, first.Code)

	pending, completed, total := f.store.counts()
	assert.Equal(t, 1, f.store.maxPending, "concurrent same-key requests hold at most one pending record")
	assert.Zero(t, pending)
	assert.Equal(t, 1, completed)
	assert.Equal(t, 1, total)
}

// --- record hygiene -------------------------------------------------------------

func TestStage5InvalidRequestCreatesNoRecord(t *testing.T) {
	f := newStage5Fixture(t, 100)
	rec := f.postChatKeyed(t, `{"model":"m","messages":[{"role":"robot","content":"x"}]}`, "invalid-key", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	_, _, total := f.store.counts()
	assert.Zero(t, total, "invalid requests must never create pending records")
}

func TestStage5RateLimitedRequestCreatesNoRecord(t *testing.T) {
	f := newStage5Fixture(t, 1)
	require.Equal(t, http.StatusOK, f.postChatKeyed(t, validChatBody, "", "").Code) // spend the window

	rec := f.postChatKeyed(t, validChatBody, "limited-key", "")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	_, _, total := f.store.counts()
	assert.Zero(t, total, "the rate limit runs before idempotency Begin")
}

func TestStage5RetryableErrorReleasesRecordSoRetryIsFresh(t *testing.T) {
	// A transient 5xx must NOT be cached under the idempotency key: the record
	// is released so a same-key retry (exactly what a retry-safe client does)
	// gets a fresh attempt against a gateway that may have recovered — rather
	// than the failure being replayed for the whole TTL.
	f := newStage5Fixture(t, 100)
	var down atomic.Bool
	down.Store(true)
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		atomic.AddInt64(&f.backends, 1)
		if down.Load() {
			return nil, &inference.Error{Backend: "mock-llm-fast", Status: 503, Transient: true}
		}
		return &inference.Response{StatusCode: http.StatusOK, Body: []byte(upstreamJSON)}, nil
	})
	f.rebuild()

	first := f.postChatKeyed(t, validChatBody, "err-key", "")
	assert.Equal(t, http.StatusServiceUnavailable, first.Code)
	pending, completed, total := f.store.counts()
	assert.Zero(t, pending, "the opened record is resolved, never left pending")
	assert.Zero(t, completed, "a retryable 5xx is not stored as a completed replay")
	assert.Zero(t, total, "the record is released, so nothing is cached under the key")

	// The backend recovers; the SAME key now succeeds instead of replaying 503.
	down.Store(false)
	calls := f.backendCalls()
	second := f.postChatKeyed(t, validChatBody, "err-key", "")
	assert.Equal(t, http.StatusOK, second.Code,
		"a same-key retry after a transient failure gets a fresh attempt")
	assert.Greater(t, f.backendCalls(), calls, "the retry actually reaches the backend")

	pending, completed, _ = f.store.counts()
	assert.Zero(t, pending)
	assert.Equal(t, 1, completed, "the successful retry is now the stored, replayable outcome")
}

func TestStage5DeterministicClientErrorIsReplayed(t *testing.T) {
	// A 404 (model not served by any backend) is deterministic client-side:
	// unlike a 5xx it IS completed and replayed, and never reaches a backend.
	f := newStage5Fixture(t, 100)
	f.selector.err = routing.ErrNoBackends
	f.rebuild()

	first := f.postChatKeyed(t, validChatBody, "notfound-key", "")
	require.Equal(t, http.StatusNotFound, first.Code)
	_, completed, _ := f.store.counts()
	assert.Equal(t, 1, completed, "a deterministic 4xx is completed for replay")

	second := f.postChatKeyed(t, validChatBody, "notfound-key", "")
	assert.Equal(t, http.StatusNotFound, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String(), "the 404 replays deterministically")
	assert.Equal(t, int64(0), f.backendCalls())
}

func TestStage5OversizedIdempotencyKeyRejected(t *testing.T) {
	f := newStage5Fixture(t, 100)
	huge := strings.Repeat("k", 256)
	rec := f.postChatKeyed(t, validChatBody, huge, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "bad_request", decodeError(t, rec.Body).Error.Code)
	_, _, total := f.store.counts()
	assert.Zero(t, total, "an oversized key never creates a record")
	assert.Equal(t, int64(0), f.backendCalls(), "and never reaches a backend")
}

// --- /v1/models shares the per-key budget ---------------------------------------

func TestStage5ModelsRouteRateLimited(t *testing.T) {
	f := newStage5Fixture(t, 1)

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+testRawKey)
		return do(t, f.handler, req)
	}
	require.Equal(t, http.StatusOK, get().Code)

	rec := get()
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "rate_limited", decodeError(t, rec.Body).Error.Code)

	f.redis.FastForward(time.Second + time.Millisecond)
	assert.Equal(t, http.StatusOK, get().Code, "the window refills after it expires")
}

// --- eligibility surfaces on the wire -------------------------------------------

func TestStage5BypassHeaderForIneligibleRequests(t *testing.T) {
	f := newStage5Fixture(t, 100)

	// Omitted temperature → effective 1.0 → BYPASS, and nothing is stored:
	// the identical follow-up is BYPASS again, both calls hit the backend.
	rec := f.postChatKeyed(t, validChatBody, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "BYPASS", rec.Header().Get("X-AegisRoute-Cache"))

	rec = f.postChatKeyed(t, validChatBody, "", "")
	assert.Equal(t, "BYPASS", rec.Header().Get("X-AegisRoute-Cache"))
	assert.Equal(t, int64(2), f.backendCalls(), "BYPASS requests always reach a backend")

	// temperature above the 0.2 threshold also bypasses.
	warm := `{"model":"llama-fast","temperature":0.3,"messages":[{"role":"user","content":"hello"}]}`
	rec = f.postChatKeyed(t, warm, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "BYPASS", rec.Header().Get("X-AegisRoute-Cache"))
}

func TestStage5CacheHitUsesCurrentRequestID(t *testing.T) {
	f := newStage5Fixture(t, 100)

	require.Equal(t, http.StatusOK, f.postChatKeyed(t, cacheableBody, "", "first-request").Code)

	rec := f.postChatKeyed(t, cacheableBody, "", "second-request")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "HIT", rec.Header().Get("X-AegisRoute-Cache"))
	assert.Equal(t, "second-request", rec.Header().Get("X-Request-ID"),
		"a HIT carries the current request's X-Request-ID, never a stored one")
}
