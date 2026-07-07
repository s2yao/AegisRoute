package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/redisstore"
)

// validBatchBody is the locked MVP create shape with one item.
const validBatchBody = `{"requests":[{"custom_id":"req-1","body":{"model":"llama-fast","messages":[{"role":"user","content":"Say one short sentence about routing."}],"temperature":0,"max_tokens":32}}]}`

// batchFixture wires the batch handlers over an in-memory job store and queue,
// a real idempotency coordinator (so replay is exercised end to end), and two
// API keys mapping to two tenants (so cross-tenant isolation is testable).
type batchFixture struct {
	deps    api.Deps
	handler http.Handler
	store   *jobs.MemStore
	queue   *redisstore.MemQueue
	tenantA uuid.UUID
	keyA    string
	tenantB uuid.UUID
	keyB    string
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()

	tenantA, tenantB := uuid.New(), uuid.New()
	keyA, keyB := "batch-key-a", "batch-key-b"
	keys := &fakeKeyStore{keys: map[string]*models.APIKey{
		auth.HashAPIKey(testSecret, keyA): {ID: uuid.New(), TenantID: tenantA},
		auth.HashAPIKey(testSecret, keyB): {ID: uuid.New(), TenantID: tenantB},
	}}

	deps := testDeps()
	deps.Keys = keys
	deps.Jobs = store
	deps.JobQueue = queue
	// Real coordinator over the api-side in-memory idempotency store, so the
	// replay assertion goes through the same Classify path as production.
	deps.Idempotency = idempotency.NewCoordinator(newFakeIdemStore(), time.Hour, time.Minute)

	return &batchFixture{
		deps:    deps,
		handler: api.NewRouter(deps),
		store:   store,
		queue:   queue,
		tenantA: tenantA,
		keyA:    keyA,
		tenantB: tenantB,
		keyB:    keyB,
	}
}

func (f *batchFixture) post(t *testing.T, rawKey, body, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batch-jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return do(t, f.handler, req)
}

func (f *batchFixture) get(t *testing.T, rawKey, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	return do(t, f.handler, req)
}

// createResponse is the create-path JSON shape.
type createResponse struct {
	ID             string `json:"id"`
	Object         string `json:"object"`
	Status         string `json:"status"`
	TotalItems     int    `json:"total_items"`
	CompletedItems int    `json:"completed_items"`
	FailedItems    int    `json:"failed_items"`
}

func decodeCreate(t *testing.T, rec *httptest.ResponseRecorder) createResponse {
	t.Helper()
	var out createResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestBatchCreate_PersistsItemsOutboxAndPublishesOnce(t *testing.T) {
	f := newBatchFixture(t)
	body := `{"requests":[
		{"custom_id":"a","body":{"model":"llama-fast","messages":[{"role":"user","content":"one"}],"temperature":0}},
		{"custom_id":"b","body":{"model":"llama-fast","messages":[{"role":"user","content":"two"}],"temperature":0}}
	]}`

	rec := f.post(t, f.keyA, body, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	got := decodeCreate(t, rec)
	assert.Equal(t, "batch_job", got.Object)
	assert.Equal(t, "queued", got.Status)
	assert.Equal(t, 2, got.TotalItems)
	assert.Equal(t, 0, got.CompletedItems)
	assert.Equal(t, 0, got.FailedItems)
	jobID, err := uuid.Parse(got.ID)
	require.NoError(t, err)

	// The job, its two items, and one outbox row are persisted for the tenant.
	job, err := f.store.Get(context.Background(), f.tenantA, jobID)
	require.NoError(t, err)
	assert.Equal(t, "llama-fast", job.Model)
	items, err := f.store.Items(context.Background(), f.tenantA, jobID)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.ElementsMatch(t, []string{"a", "b"},
		[]string{items[0].CustomID, items[1].CustomID})

	// Exactly one logical publish for the whole job (never one per item), and
	// the outbox row is marked published (no longer pending).
	assert.Equal(t, 1, f.queue.PublishCount(), "exactly one job-level publish")
	assert.Equal(t, []string{jobID.String()}, f.queue.Published())
	pending, err := f.store.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending, "successful publish marks the outbox row published")
}

func TestBatchCreate_PublishFailureLeavesOutboxPending(t *testing.T) {
	f := newBatchFixture(t)
	f.queue.SetPublishErr(fmt.Errorf("redis down"))

	rec := f.post(t, f.keyA, validBatchBody, "")
	// The job is durably committed regardless of the publish outcome, so the
	// client still gets a 201 — the outbox drain will enqueue it later.
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got := decodeCreate(t, rec)
	jobID := uuid.MustParse(got.ID)

	assert.Equal(t, 0, f.queue.PublishCount(), "publish failed, nothing enqueued")
	pending, err := f.store.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the outbox row stays pending for the drain loop")
	assert.Equal(t, jobID, pending[0].JobID)
	assert.GreaterOrEqual(t, pending[0].Attempts, 1)
}

func TestBatchGet_ReturnsStatus(t *testing.T) {
	f := newBatchFixture(t)
	rec := f.post(t, f.keyA, validBatchBody, "")
	require.Equal(t, http.StatusCreated, rec.Code)
	id := decodeCreate(t, rec).ID

	rec = f.get(t, f.keyA, "/api/v1/batch-jobs/"+id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		ID         string `json:"id"`
		Object     string `json:"object"`
		Status     string `json:"status"`
		Model      string `json:"model"`
		TotalItems int    `json:"total_items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "batch_job", got.Object)
	assert.Equal(t, "queued", got.Status)
	assert.Equal(t, "llama-fast", got.Model)
	assert.Equal(t, 1, got.TotalItems)
}

func TestBatchItems_ReturnsItems(t *testing.T) {
	f := newBatchFixture(t)
	body := `{"requests":[
		{"custom_id":"first","body":{"model":"llama-fast","messages":[{"role":"user","content":"one"}]}},
		{"custom_id":"second","body":{"model":"llama-fast","messages":[{"role":"user","content":"two"}]}}
	]}`
	rec := f.post(t, f.keyA, body, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeCreate(t, rec).ID

	rec = f.get(t, f.keyA, "/api/v1/batch-jobs/"+id+"/items")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var items []struct {
		CustomID string          `json:"custom_id"`
		Status   string          `json:"status"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 2)
	assert.Equal(t, "first", items[0].CustomID)
	assert.Equal(t, "queued", items[0].Status)
	assert.Contains(t, string(items[0].Request), "llama-fast")
	assert.Equal(t, "null", string(items[1].Response), "a queued item's response is JSON null")
}

func TestBatchList_ReturnsOnlyTenantJobs(t *testing.T) {
	f := newBatchFixture(t)
	require.Equal(t, http.StatusCreated, f.post(t, f.keyA, validBatchBody, "").Code)
	require.Equal(t, http.StatusCreated, f.post(t, f.keyA, validBatchBody, "").Code)
	require.Equal(t, http.StatusCreated, f.post(t, f.keyB, validBatchBody, "").Code)

	rec := f.get(t, f.keyA, "/api/v1/batch-jobs")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listA []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listA))
	assert.Len(t, listA, 2, "tenant A sees only its two jobs")

	rec = f.get(t, f.keyB, "/api/v1/batch-jobs")
	require.Equal(t, http.StatusOK, rec.Code)
	var listB []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listB))
	assert.Len(t, listB, 1, "tenant B sees only its one job")
}

func TestBatchGet_OtherTenantIsNotFound(t *testing.T) {
	f := newBatchFixture(t)
	rec := f.post(t, f.keyA, validBatchBody, "")
	require.Equal(t, http.StatusCreated, rec.Code)
	id := decodeCreate(t, rec).ID

	// Tenant B must not be able to read tenant A's job or its items — the
	// same 404 as a nonexistent id, so existence is never leaked.
	rec = f.get(t, f.keyB, "/api/v1/batch-jobs/"+id)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", decodeError(t, rec.Body).Error.Code)

	rec = f.get(t, f.keyB, "/api/v1/batch-jobs/"+id+"/items")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBatchCreate_IdempotencyKeyReplays(t *testing.T) {
	f := newBatchFixture(t)

	first := f.post(t, f.keyA, validBatchBody, "idem-batch-1")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	firstBody := decodeCreate(t, first)

	// Same key + same body: the stored response is replayed verbatim, no
	// second job is created, and no second publish happens.
	second := f.post(t, f.keyA, validBatchBody, "idem-batch-1")
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
	secondBody := decodeCreate(t, second)

	assert.Equal(t, firstBody.ID, secondBody.ID, "replay returns the original job id")
	list, err := f.store.List(context.Background(), f.tenantA)
	require.NoError(t, err)
	assert.Len(t, list, 1, "the replay must not create a second job")
	assert.Equal(t, 1, f.queue.PublishCount(), "the replay must not publish again")
}

func TestBatchCreate_Validation(t *testing.T) {
	f := newBatchFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty requests", `{"requests":[]}`},
		{"missing requests", `{}`},
		{"unsupported top-level field", `{"requests":[{"custom_id":"a","body":{"model":"m","messages":[{"role":"user","content":"x"}]}}],"extra":1}`},
		{"missing custom_id", `{"requests":[{"body":{"model":"llama-fast","messages":[{"role":"user","content":"x"}]}}]}`},
		{"blank custom_id", `{"requests":[{"custom_id":"  ","body":{"model":"llama-fast","messages":[{"role":"user","content":"x"}]}}]}`},
		{"duplicate custom_id", `{"requests":[
			{"custom_id":"dup","body":{"model":"llama-fast","messages":[{"role":"user","content":"x"}]}},
			{"custom_id":"dup","body":{"model":"llama-fast","messages":[{"role":"user","content":"y"}]}}
		]}`},
		{"missing body", `{"requests":[{"custom_id":"a"}]}`},
		{"invalid body role", `{"requests":[{"custom_id":"a","body":{"model":"llama-fast","messages":[{"role":"bogus","content":"x"}]}}]}`},
		{"mixed models", `{"requests":[
			{"custom_id":"a","body":{"model":"llama-fast","messages":[{"role":"user","content":"x"}]}},
			{"custom_id":"b","body":{"model":"llama-cheap","messages":[{"role":"user","content":"y"}]}}
		]}`},
		{"streaming body rejected", `{"requests":[{"custom_id":"a","body":{"model":"llama-fast","stream":true,"messages":[{"role":"user","content":"x"}]}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.post(t, f.keyA, tc.body, "")
			assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
			assert.Less(t, rec.Code, 500, "validation failures are client errors")
			// A rejected create must never persist a job or publish anything.
			list, err := f.store.List(context.Background(), f.tenantA)
			require.NoError(t, err)
			assert.Empty(t, list)
			assert.Equal(t, 0, f.queue.PublishCount())
		})
	}
}

func TestBatchCreate_TooManyRequests(t *testing.T) {
	f := newBatchFixture(t)
	var sb strings.Builder
	sb.WriteString(`{"requests":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"custom_id":"c%d","body":{"model":"llama-fast","messages":[{"role":"user","content":"x"}]}}`, i)
	}
	sb.WriteString(`]}`)

	rec := f.post(t, f.keyA, sb.String(), "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, decodeError(t, rec.Body).Error.Message, "100")
}

func TestBatchCreate_Unauthenticated(t *testing.T) {
	f := newBatchFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batch-jobs", strings.NewReader(validBatchBody))
	rec := do(t, f.handler, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
