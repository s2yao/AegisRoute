package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/redisstore"
	"github.com/example/aegisroute/internal/routing"
)

// --- fakes -----------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSelector always hands back one fixed backend and a no-op release.
type fakeSelector struct {
	backend models.ModelBackend
	err     error
}

func (f fakeSelector) Select(_ context.Context, _ string, _ ...uuid.UUID) (routing.Selection, func(), error) {
	if f.err != nil {
		return routing.Selection{}, nil, f.err
	}
	return routing.Selection{Backend: f.backend, PolicyName: "default", Strategy: "priority_weighted"}, func() {}, nil
}

// peakInference records the maximum number of concurrent Do calls and the
// total call count, so a test can assert the bounded pool never exceeds its
// limit and that redelivery does not re-run items.
type peakInference struct {
	delay time.Duration
	mu    sync.Mutex
	cur   int
	peak  int
	calls int
	// failWith, when set, is returned instead of a success (same value for
	// every call) — used to drive item-failure paths.
	failWith error
}

func (p *peakInference) Do(_ context.Context, _ models.ModelBackend, _ []byte) (*inference.Response, error) {
	p.mu.Lock()
	p.cur++
	p.calls++
	if p.cur > p.peak {
		p.peak = p.cur
	}
	p.mu.Unlock()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}

	p.mu.Lock()
	p.cur--
	p.mu.Unlock()

	if p.failWith != nil {
		return nil, p.failWith
	}
	return &inference.Response{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
}

func (p *peakInference) Peak() int  { p.mu.Lock(); defer p.mu.Unlock(); return p.peak }
func (p *peakInference) Calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

// noopCircuit satisfies Circuit without recording anything.
type noopCircuit struct{}

func (noopCircuit) ReportSuccess(string)  {}
func (noopCircuit) ReportFailure(string)  {}
func (noopCircuit) ReportCanceled(string) {}

// recorder captures an ordered log of store/queue events shared by the
// instrumented wrappers, so a test can assert Ack lands strictly after the
// durable Postgres updates.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) indexOf(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if e == s {
			return i
		}
	}
	return -1
}

// recordingStore wraps MemStore, recording the two durable writes that must
// precede an Ack.
type recordingStore struct {
	*jobs.MemStore
	rec *recorder
}

func (s recordingStore) UpdateItemTerminal(ctx context.Context, itemID uuid.UUID, status models.ItemStatus, response json.RawMessage, errMsg *string) error {
	err := s.MemStore.UpdateItemTerminal(ctx, itemID, status, response, errMsg)
	if err == nil {
		s.rec.add("UpdateItemTerminal")
	}
	return err
}

func (s recordingStore) RecomputeAndUpdateJobStatus(ctx context.Context, jobID uuid.UUID) (models.JobStatus, error) {
	st, err := s.MemStore.RecomputeAndUpdateJobStatus(ctx, jobID)
	if err == nil {
		s.rec.add("Recompute")
	}
	return st, err
}

// recordingQueue wraps MemQueue, recording Ack.
type recordingQueue struct {
	*redisstore.MemQueue
	rec *recorder
}

func (q recordingQueue) Ack(ctx context.Context, msg redisstore.Message) error {
	q.rec.add("Ack")
	return q.MemQueue.Ack(ctx, msg)
}

// --- helpers ---------------------------------------------------------------

func makeItems(n int) []models.BatchJobItem {
	out := make([]models.BatchJobItem, n)
	for i := 0; i < n; i++ {
		out[i] = models.BatchJobItem{
			CustomID: "req-" + uuid.NewString(),
			Request:  json.RawMessage(`{"model":"llama-fast","messages":[{"role":"user","content":"hi"}]}`),
		}
	}
	return out
}

func testWorker(t *testing.T, deps Deps, cfg Config) *Worker {
	t.Helper()
	if deps.Selector == nil {
		deps.Selector = fakeSelector{backend: models.ModelBackend{ID: uuid.New(), Name: "fast"}}
	}
	if deps.Circuit == nil {
		deps.Circuit = noopCircuit{}
	}
	if deps.Logger == nil {
		deps.Logger = discardLogger()
	}
	if deps.Metrics == nil {
		deps.Metrics = metrics.New()
	}
	return New(deps, cfg)
}

// --- tests -----------------------------------------------------------------

func TestWorker_ConcurrencyNeverExceedsLimit(t *testing.T) {
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	inf := &peakInference{delay: 10 * time.Millisecond}
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: inf}, Config{Concurrency: 4, MaxItemAttempts: 3})

	job, _, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(60))
	require.NoError(t, err)

	require.NoError(t, w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: job.ID.String()}))

	assert.LessOrEqual(t, inf.Peak(), 4, "bounded pool must never exceed WORKER_CONCURRENCY")
	assert.GreaterOrEqual(t, inf.Peak(), 2, "pool should actually run items in parallel")
	assert.Equal(t, 60, inf.Calls(), "every item processed exactly once")

	status, err := store.RecomputeAndUpdateJobStatus(context.Background(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, models.JobStatusSucceeded, status)
}

func TestWorker_RedeliveryDoesNotReprocessTerminalItems(t *testing.T) {
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	inf := &peakInference{}
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: inf}, Config{Concurrency: 4, MaxItemAttempts: 3})

	job, _, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(10))
	require.NoError(t, err)
	msg := redisstore.Message{ID: "m1", JobID: job.ID.String()}

	require.NoError(t, w.handleMessage(context.Background(), msg))
	assert.Equal(t, 10, inf.Calls())

	// Redeliver the same message (at-least-once): all items are already
	// terminal, so none are claimable and inference is not called again.
	require.NoError(t, w.handleMessage(context.Background(), msg))
	assert.Equal(t, 10, inf.Calls(), "redelivery must not re-run terminal items")
}

func TestWorker_AckOnlyAfterDurableUpdate(t *testing.T) {
	rec := &recorder{}
	base := jobs.NewMemStore()
	store := recordingStore{MemStore: base, rec: rec}
	queue := recordingQueue{MemQueue: redisstore.NewMemQueue(), rec: rec}
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: &peakInference{}}, Config{Concurrency: 1, MaxItemAttempts: 3})

	job, _, err := base.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(1))
	require.NoError(t, err)

	require.NoError(t, w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: job.ID.String()}))

	update := rec.indexOf("UpdateItemTerminal")
	recompute := rec.indexOf("Recompute")
	ack := rec.indexOf("Ack")
	require.NotEqual(t, -1, update, "item must be durably written")
	require.NotEqual(t, -1, ack, "message must be acked")
	assert.Less(t, update, ack, "Ack must come after the durable item update")
	assert.Less(t, recompute, ack, "Ack must come after the job-status recompute")

	// And the message really was acked exactly once.
	assert.Len(t, queue.Acked(), 1)
}

func TestWorker_ExhaustedItemGoesToDLQAndJobFails(t *testing.T) {
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: &peakInference{}}, Config{Concurrency: 1, MaxItemAttempts: 1})

	job, _, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(1))
	require.NoError(t, err)

	// Drive the single item to attempts == MaxItemAttempts while leaving it
	// queued (the crash-recovery shape: claimed then requeued without a
	// terminal write). The next claim inside handleMessage must exhaust it.
	res, err := store.ClaimNextQueuedItem(context.Background(), job.ID, 1)
	require.NoError(t, err)
	require.Equal(t, jobs.ClaimClaimed, res.Outcome)
	_, err = store.RequeueRunningItems(context.Background(), job.ID)
	require.NoError(t, err)

	require.NoError(t, w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: job.ID.String()}))

	// Exactly one DLQ entry for the exhausted item.
	dlq := queue.DLQ()
	require.Len(t, dlq, 1)
	assert.Equal(t, job.ID.String(), dlq[0].Msg.JobID)
	assert.Contains(t, dlq[0].Reason, "exhausted")

	// The item is terminally failed and the (single-item) job is failed.
	got, err := store.Get(context.Background(), job.TenantID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, models.JobStatusFailed, got.Status)
	assert.Equal(t, 1, got.FailedItems)

	// The message is still acked: the pass completed (the item reached a
	// terminal state), so the message must not circulate forever.
	assert.Len(t, queue.Acked(), 1)
}

func TestWorker_PartialFailureAggregates(t *testing.T) {
	// One item fails permanently at inference, one succeeds, so the job
	// aggregates to partially_failed. Concurrency 1 makes item processing
	// deterministic (claim order), so the first backend call maps to the
	// first item and the second call to the second.
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	inf := &sequencedInference{results: []scriptedResult{
		{err: &inference.Error{Backend: "fast", Transient: false, Status: 400}},
		{body: []byte(`{"ok":true}`)},
	}}
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: inf}, Config{Concurrency: 1, MaxItemAttempts: 3})
	job, _, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(2))
	require.NoError(t, err)

	require.NoError(t, w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: job.ID.String()}))

	got, err := store.Get(context.Background(), job.TenantID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, models.JobStatusPartiallyFailed, got.Status)
	assert.Equal(t, 1, got.CompletedItems)
	assert.Equal(t, 1, got.FailedItems)
}

func TestWorker_OutboxDrainRepublishesPendingRows(t *testing.T) {
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: &peakInference{}}, Config{Concurrency: 1, MaxItemAttempts: 3})

	job, ob, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(1))
	require.NoError(t, err)

	// First drain: publish fails, so the row stays pending and is NOT marked
	// published — the correctness gate the prompt calls out.
	queue.SetPublishErr(errors.New("redis down"))
	w.drainOutboxOnce(context.Background())
	assert.Equal(t, 0, queue.PublishCount())
	pending, err := store.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, ob.ID, pending[0].ID)
	assert.GreaterOrEqual(t, pending[0].Attempts, 1)

	// Second drain: publish succeeds, the row is marked published exactly
	// after the successful Publish, and drops out of the pending set.
	queue.SetPublishErr(nil)
	w.drainOutboxOnce(context.Background())
	assert.Equal(t, 1, queue.PublishCount())
	assert.Equal(t, []string{job.ID.String()}, queue.Published())
	pending, err = store.PendingOutbox(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// sequencedInference returns results in call order, one per invocation.
// Paired with Concurrency: 1 it makes per-item outcomes deterministic. A
// permanent inference.Error never fails over, so each item consumes exactly
// one result.
type sequencedInference struct {
	mu      sync.Mutex
	results []scriptedResult
	idx     int
}

type scriptedResult struct {
	body []byte
	err  error
}

func (s *sequencedInference) Do(_ context.Context, _ models.ModelBackend, _ []byte) (*inference.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.results) {
		return &inference.Response{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	r := s.results[s.idx]
	s.idx++
	if r.err != nil {
		return nil, r.err
	}
	return &inference.Response{StatusCode: 200, Body: r.body}, nil
}

func TestWorker_MissingJobIsDroppedNotLooped(t *testing.T) {
	// A message whose job no longer exists (its tenant was deleted, cascading)
	// must be dead-lettered and acked, not left to redeliver forever. With the
	// MemStore, MarkJobRunning reports ErrNotFound for an unknown job.
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: &peakInference{}}, Config{Concurrency: 1, MaxItemAttempts: 3})

	missing := uuid.New()
	err := w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: missing.String()})
	require.NoError(t, err, "a vanished job must not fail the pass forever")

	assert.Len(t, queue.Acked(), 1, "the moot message is acked (dropped)")
	dlq := queue.DLQ()
	require.Len(t, dlq, 1)
	assert.Contains(t, dlq[0].Reason, "no longer exists")
}

func TestWorker_InFlightGuardRefusesConcurrentDoubleHandling(t *testing.T) {
	// The consume loop and the reclaim loop can both surface the same delivery;
	// the second entrant must be a no-op so one job is never processed twice at
	// once in this process.
	w := testWorker(t, Deps{Queue: redisstore.NewMemQueue(), Store: jobs.NewMemStore(), Inference: &peakInference{}}, Config{})

	release, ok := w.markInFlight("m1")
	require.True(t, ok)
	_, ok = w.markInFlight("m1")
	assert.False(t, ok, "the same delivery cannot be marked in-flight twice")
	assert.True(t, w.isInFlight("m1"))

	release()
	assert.False(t, w.isInFlight("m1"))
	_, ok = w.markInFlight("m1")
	assert.True(t, ok, "after release the delivery can be handled again")
}

func TestWorker_HandleMessageSkipsAlreadyInFlightDelivery(t *testing.T) {
	// If a delivery is already being handled by this process, a second
	// handleMessage for it does nothing — no store touch, no ack. Uses a
	// nonexistent job so that, absent the guard, the second call WOULD ack
	// (drop-missing), making the skip observable.
	store := jobs.NewMemStore()
	queue := redisstore.NewMemQueue()
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: &peakInference{}}, Config{})

	release, ok := w.markInFlight("m1")
	require.True(t, ok)
	defer release()

	err := w.handleMessage(context.Background(), redisstore.Message{ID: "m1", JobID: uuid.NewString()})
	require.NoError(t, err)
	assert.Empty(t, queue.Acked(), "an already-in-flight delivery must be skipped, not acked")
	assert.Empty(t, queue.DLQ())
}

func TestWorker_EndToEndOverStreamQueue(t *testing.T) {
	// The real integration: worker.Run driving the actual Redis Streams consume
	// loop (miniredis) end to end — publish a job id, let the worker consume,
	// process every item through routing+inference, and ack. Unit tests drive
	// handleMessage directly; this proves the Consume→handler→Ack wiring works.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	streams := redisstore.Streams{Key: "aegis:e2e", Group: "workers"}
	queue := redisstore.NewStreamQueue(rdb, streams, "consumer-1", redisstore.WithBlock(50*time.Millisecond))

	store := jobs.NewMemStore()
	inf := &peakInference{}
	w := testWorker(t, Deps{Queue: queue, Store: store, Inference: inf}, Config{Concurrency: 4, MaxItemAttempts: 3})

	job, _, err := store.CreateWithItemsAndOutbox(context.Background(),
		models.BatchJob{TenantID: uuid.New(), APIKeyID: uuid.New(), Model: "llama-fast"}, makeItems(5))
	require.NoError(t, err)
	require.NoError(t, queue.Publish(context.Background(), job.ID.String()))

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(runCtx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		got, err := store.Get(context.Background(), job.TenantID, job.ID)
		return err == nil && got.Status == models.JobStatusSucceeded
	}, 5*time.Second, 20*time.Millisecond, "the worker must drive the job to succeeded")

	assert.Equal(t, 5, inf.Calls(), "each item processed exactly once")
}
