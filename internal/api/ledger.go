package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/example/aegisroute/internal/models"
)

// LedgerRecorder records one inference_requests row. It is fire-and-forget:
// Record never blocks the caller and never returns an error, so persisting
// the audit trail can never add latency to — or fail — a served completion.
// Satisfied by *AsyncLedger in production and by synchronous fakes in tests.
type LedgerRecorder interface {
	Record(row models.InferenceRequest)
}

// ledgerInsertTimeout bounds one background insert. It is independent of any
// request's lifetime: the write outlives the request that produced it (a
// client disconnect must not lose the audit row).
const ledgerInsertTimeout = 5 * time.Second

// AsyncLedger persists inference_requests rows on a bounded pool of
// background workers fed by a buffered queue, keeping the audit write off the
// request hot path. When the queue is full it drops the row (and logs),
// because a served completion must never wait on — or be failed by — its own
// audit trail. Records already accepted are drained on Close.
type AsyncLedger struct {
	store   InferenceRequestStore
	logger  *slog.Logger
	queue   chan models.InferenceRequest
	done    chan struct{}
	wg      sync.WaitGroup
	timeout time.Duration
}

// NewAsyncLedger starts workers background goroutines draining a queue of the
// given buffer size into store. Non-positive workers/buffer fall back to
// small defaults. Call Close on shutdown to flush accepted rows.
func NewAsyncLedger(store InferenceRequestStore, logger *slog.Logger, workers, buffer int) *AsyncLedger {
	if workers < 1 {
		workers = 4
	}
	if buffer < 1 {
		buffer = 1024
	}
	l := &AsyncLedger{
		store:   store,
		logger:  logger,
		queue:   make(chan models.InferenceRequest, buffer),
		done:    make(chan struct{}),
		timeout: ledgerInsertTimeout,
	}
	l.wg.Add(workers)
	for range workers {
		go l.worker()
	}
	return l
}

// Record enqueues row for a background insert. It never blocks: if the queue
// is full (Postgres is slower than the request rate) or the ledger is
// closing, the row is dropped with a warning rather than stalling the caller.
func (l *AsyncLedger) Record(row models.InferenceRequest) {
	select {
	case l.queue <- row:
	default:
		l.logger.Warn("inference ledger row dropped: queue full",
			"backend_id", row.BackendID, "status", row.Status)
	}
}

// worker drains the queue until Close signals done, then drains whatever
// remains without blocking and exits.
func (l *AsyncLedger) worker() {
	defer l.wg.Done()
	for {
		select {
		case row := <-l.queue:
			l.insert(row)
		case <-l.done:
			for {
				select {
				case row := <-l.queue:
					l.insert(row)
				default:
					return
				}
			}
		}
	}
}

// insert performs one best-effort write on a background context; a failure is
// logged, never surfaced.
func (l *AsyncLedger) insert(row models.InferenceRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	if _, err := l.store.Insert(ctx, row); err != nil {
		l.logger.Error("failed to persist inference request",
			"backend_id", row.BackendID, "status", row.Status, "error", err)
	}
}

// Close stops the workers after they drain the rows already queued, bounding
// how long shutdown waits so a stuck Postgres cannot hang the process. Rows
// arriving after Close are dropped. Close is safe to call once.
func (l *AsyncLedger) Close() {
	close(l.done)
	drained := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(l.timeout + time.Second):
		l.logger.Warn("inference ledger close timed out; some rows may be unwritten")
	}
}
