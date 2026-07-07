package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/redisstore"
	"github.com/example/aegisroute/internal/routing"
)

// Selector picks a backend for a model and reserves an in-flight slot on it,
// exactly like the gateway's chat path — the worker shares internal/routing,
// it never calls gateway-api over HTTP. Satisfied by *routing.Selector.
type Selector interface {
	Select(ctx context.Context, model string, exclude ...uuid.UUID) (routing.Selection, func(), error)
}

// Inference executes one outbound backend call with retry and timeout.
// Satisfied by *inference.Client.
type Inference interface {
	Do(ctx context.Context, backend models.ModelBackend, body []byte) (*inference.Response, error)
}

// Circuit receives per-backend call outcomes; it must be the same breaker
// instance the Selector consults. Satisfied by *routing.Breaker.
type Circuit interface {
	ReportSuccess(backend string)
	ReportFailure(backend string)
	ReportCanceled(backend string)
}

// Config bounds the worker's loops. Zero fields select the defaults below.
type Config struct {
	// Concurrency sizes the bounded item-processing pool (WORKER_CONCURRENCY).
	Concurrency int
	// MaxItemAttempts bounds how many times one item may be claimed across
	// deliveries before it is exhausted to the DLQ (WORKER_MAX_ITEM_ATTEMPTS).
	MaxItemAttempts int
	// ReclaimMinIdle is how long a delivered, un-acked message must sit idle
	// before the claim loop steals it from a (presumed dead) consumer.
	ReclaimMinIdle time.Duration
	// ReclaimInterval is how often the stream-claim recovery loop runs.
	ReclaimInterval time.Duration
	// OutboxInterval is how often the outbox drain runs.
	OutboxInterval time.Duration
	// OutboxBatch caps how many pending outbox rows one drain pass publishes.
	OutboxBatch int
}

// Defaults for Config's zero fields. ReclaimMinIdle must comfortably exceed
// a normal job's processing time: a message reclaimed from a merely-slow
// consumer is processed twice (harmless — idempotent — but wasteful).
const (
	defaultConcurrency     = 1
	defaultMaxItemAttempts = 1
	defaultReclaimMinIdle  = 60 * time.Second
	defaultReclaimInterval = 30 * time.Second
	defaultOutboxInterval  = 5 * time.Second
	defaultOutboxBatch     = 100
	// consumeRetryBackoff paces re-entering the consume loop after it fails
	// (e.g. Redis unreachable while ensuring the consumer group).
	consumeRetryBackoff = time.Second
)

// Deps is everything New needs. The composition root (cmd/control-worker)
// fills it from the real queue, repo, selector, and client; tests fill it
// with fakes.
type Deps struct {
	Queue     redisstore.Queue
	Store     jobs.JobStore
	Selector  Selector
	Inference Inference
	Circuit   Circuit
	Logger    *slog.Logger
	Metrics   *metrics.Metrics
}

// Worker consumes job-level messages and processes their items concurrently
// and durably: claim an item in Postgres, run inference, write the terminal
// result, and only after every claimable item is terminal — and the job
// status recomputed — Ack the message. Redis Streams delivery is
// at-least-once, so all of this is idempotent per item: terminal items are
// never claimable again.
type Worker struct {
	queue     redisstore.Queue
	store     jobs.JobStore
	selector  Selector
	inference Inference
	circuit   Circuit
	logger    *slog.Logger
	metrics   *metrics.Metrics
	cfg       Config

	// inflight tracks the delivery IDs this process is currently handling, so
	// the reclaim loop never steals a message the consume loop is still
	// processing. Without it, a job whose single delivery runs longer than
	// ReclaimMinIdle would be XAUTOCLAIMed back to this same consumer and
	// processed a second time concurrently — harmless for item results
	// (terminal items are immutable) but it could prematurely exhaust an item
	// (requeue + re-claim inflates attempts) that was actually succeeding.
	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

// New builds a Worker, filling zero Config fields with defaults.
func New(deps Deps, cfg Config) *Worker {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.MaxItemAttempts < 1 {
		cfg.MaxItemAttempts = defaultMaxItemAttempts
	}
	if cfg.ReclaimMinIdle <= 0 {
		cfg.ReclaimMinIdle = defaultReclaimMinIdle
	}
	if cfg.ReclaimInterval <= 0 {
		cfg.ReclaimInterval = defaultReclaimInterval
	}
	if cfg.OutboxInterval <= 0 {
		cfg.OutboxInterval = defaultOutboxInterval
	}
	if cfg.OutboxBatch < 1 {
		cfg.OutboxBatch = defaultOutboxBatch
	}
	m := deps.Metrics
	if m == nil {
		m = metrics.New()
	}
	return &Worker{
		queue:     deps.Queue,
		store:     deps.Store,
		selector:  deps.Selector,
		inference: deps.Inference,
		circuit:   deps.Circuit,
		logger:    deps.Logger,
		metrics:   m,
		cfg:       cfg,
		inflight:  map[string]struct{}{},
	}
}

// markInFlight records that this process is handling msgID and returns a
// release func to clear it (deferred by handleMessage). A message already in
// flight (a reclaim of one the consume loop is still processing) returns
// ok=false and must be skipped by the caller.
func (w *Worker) markInFlight(msgID string) (release func(), ok bool) {
	w.inflightMu.Lock()
	defer w.inflightMu.Unlock()
	if _, exists := w.inflight[msgID]; exists {
		return func() {}, false
	}
	w.inflight[msgID] = struct{}{}
	return func() {
		w.inflightMu.Lock()
		delete(w.inflight, msgID)
		w.inflightMu.Unlock()
	}, true
}

// Run drives the three loops — consume, stream reclaim, outbox drain — until
// ctx ends, then waits for them to wind down. In-flight item work observes
// the same ctx: a shutdown mid-job leaves the message un-acked, and a later
// delivery resumes exactly where the item table says it stopped.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		w.consumeLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		w.tick(ctx, w.cfg.ReclaimInterval, w.reclaimOnce)
	}()
	go func() {
		defer wg.Done()
		w.tick(ctx, w.cfg.OutboxInterval, w.drainOutboxOnce)
	}()
	wg.Wait()
	return nil
}

// consumeLoop keeps the queue consumer alive until ctx ends, re-entering
// Consume after transient failures (e.g. Redis down while ensuring the
// consumer group at boot) instead of letting one error kill the worker.
func (w *Worker) consumeLoop(ctx context.Context) {
	for {
		err := w.queue.Consume(ctx, func(ctx context.Context, msg redisstore.Message) error {
			return w.handleMessage(ctx, msg)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.metrics.WorkerFailuresTotal.Inc()
			w.logger.Error("consume loop failed; retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(consumeRetryBackoff):
		}
	}
}

// tick runs fn every interval until ctx ends.
func (w *Worker) tick(ctx context.Context, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// handleMessage processes one job-level message end to end. Returning an
// error means the message is NOT acked — it stays pending and is retried by
// a later delivery. The order is load-bearing: every durable Postgres update
// happens before the final Ack, which is what makes a crash at any point
// recoverable instead of lossy.
func (w *Worker) handleMessage(ctx context.Context, msg redisstore.Message) error {
	// Guard against this process handling the same delivery twice at once:
	// the consume loop and the reclaim loop can both surface the same message
	// (a long-running job's delivery goes idle and XAUTOCLAIM returns it while
	// the consume loop is still working it). The second entrant skips.
	release, ok := w.markInFlight(msg.ID)
	if !ok {
		return nil
	}
	defer release()

	jobID, err := uuid.Parse(msg.JobID)
	if err != nil {
		// A message that can never be processed must not circulate forever:
		// dead-letter it, then ack. If even the DLQ publish fails, leave the
		// message pending and let a later claim retry the whole sequence.
		w.metrics.WorkerFailuresTotal.Inc()
		w.logger.Error("job message has no usable job id; dead-lettering",
			"message_id", msg.ID, "job_id", msg.JobID, "error", err)
		if dlqErr := w.queue.PublishDLQ(ctx, msg, fmt.Sprintf("unparseable job_id %q", msg.JobID)); dlqErr != nil {
			return dlqErr
		}
		return w.queue.Ack(ctx, msg)
	}

	if err := w.store.MarkJobRunning(ctx, jobID); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			// The job was deleted (e.g. its tenant was removed, cascading) after
			// the message was published. It can never be processed, so drop it
			// rather than let it redeliver forever.
			return w.dropMissingJob(ctx, msg, jobID)
		}
		return w.failJobPass(jobID, "mark job running", err)
	}
	// Items still 'running' at delivery time were stranded mid-flight by a
	// dead (or reclaimed slow) worker; requeue them — attempts intact — so
	// the claim loop below can retry them. Without this, a crash between a
	// committed claim and the terminal write would strand the item forever.
	if _, err := w.store.RequeueRunningItems(ctx, jobID); err != nil {
		return w.failJobPass(jobID, "requeue running items", err)
	}

	// Bounded pool: Concurrency goroutines each repeatedly claim the next
	// queued item until none remain. The claim is the mutual exclusion —
	// two goroutines (or two worker processes) can never hold the same item.
	var (
		wg     sync.WaitGroup
		broken atomic.Bool
	)
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A panic must not kill the whole worker process: mark the pass
			// broken (no ack) and let redelivery retry the job.
			defer func() {
				if rec := recover(); rec != nil {
					broken.Store(true)
					w.metrics.WorkerFailuresTotal.Inc()
					w.logger.Error("panic while processing batch item", "job_id", jobID, "panic", rec)
				}
			}()
			for ctx.Err() == nil {
				res, err := w.store.ClaimNextQueuedItem(ctx, jobID, w.cfg.MaxItemAttempts)
				if err != nil {
					broken.Store(true)
					w.metrics.WorkerFailuresTotal.Inc()
					w.logger.Error("item claim failed", "job_id", jobID, "error", err)
					return
				}
				switch res.Outcome {
				case jobs.ClaimNone:
					return
				case jobs.ClaimExhausted:
					// The claim already durably failed the item — exactly
					// once, since terminal items are never claimable again —
					// so this is the single DLQ entry for it. A failed DLQ
					// publish is logged, not retried: the item's terminal
					// state (with the reason in its error column) is the
					// durable record.
					w.metrics.BatchItemsProcessedTotal.WithLabelValues(string(models.ItemStatusFailed)).Inc()
					reason := fmt.Sprintf("item %s (custom_id %q) %s", res.Item.ID, res.Item.CustomID, jobs.ExhaustedError(res.Item.Attempts, w.cfg.MaxItemAttempts))
					if dlqErr := w.queue.PublishDLQ(ctx, msg, reason); dlqErr != nil {
						w.metrics.WorkerFailuresTotal.Inc()
						w.logger.Error("dlq publish failed", "job_id", jobID, "item_id", res.Item.ID, "error", dlqErr)
					}
				case jobs.ClaimClaimed:
					if err := w.processItem(ctx, res.Item); err != nil {
						broken.Store(true)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err() // shutting down: no ack, redelivery resumes the job
	}
	if broken.Load() {
		return fmt.Errorf("worker: job %s pass incomplete; leaving message pending", jobID)
	}
	if _, err := w.store.RecomputeAndUpdateJobStatus(ctx, jobID); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			return w.dropMissingJob(ctx, msg, jobID)
		}
		return w.failJobPass(jobID, "recompute job status", err)
	}
	// Ack strictly last: every item outcome and the job status are already
	// durable in Postgres, so losing the process here at worst redelivers a
	// message whose items are all terminal — a cheap no-op pass.
	if err := w.queue.Ack(ctx, msg); err != nil {
		return w.failJobPass(jobID, "ack", err)
	}
	return nil
}

// failJobPass records an infrastructure failure that aborts this delivery of
// the job (the message stays pending for a retry).
func (w *Worker) failJobPass(jobID uuid.UUID, op string, err error) error {
	w.metrics.WorkerFailuresTotal.Inc()
	w.logger.Error("job pass failed", "job_id", jobID, "op", op, "error", err)
	return fmt.Errorf("worker: %s for job %s: %w", op, jobID, err)
}

// dropMissingJob dead-letters and acks a message whose job no longer exists,
// so a moot delivery (the job's tenant was deleted after publish) cannot
// redeliver forever. A failed DLQ publish leaves the message pending for a
// later retry rather than acking without a record.
func (w *Worker) dropMissingJob(ctx context.Context, msg redisstore.Message, jobID uuid.UUID) error {
	w.logger.Warn("batch job no longer exists; dropping message",
		"job_id", jobID, "message_id", msg.ID)
	if dlqErr := w.queue.PublishDLQ(ctx, msg, fmt.Sprintf("job %s no longer exists", jobID)); dlqErr != nil {
		return dlqErr
	}
	return w.queue.Ack(ctx, msg)
}

// processItem runs one claimed item to a terminal state: route, call the
// backend (failing over across backends on transient errors, exactly like
// the gateway's chat path), and durably write the result. Only a shutdown or
// a failed durable write returns an error — business failures ARE the
// terminal result, not errors.
func (w *Worker) processItem(ctx context.Context, item models.BatchJobItem) error {
	model := modelFromRequest(item.Request)
	if model == "" {
		// Validation guarantees a model at create time; a corrupt row must
		// fail terminally rather than wedge the job.
		return w.finishItem(ctx, item, models.ItemStatusFailed, nil, strPtr("stored request has no model"))
	}

	var tried []uuid.UUID
	for {
		if ctx.Err() != nil {
			// Shutdown mid-item: write nothing. The item stays running and
			// the next delivery requeues and retries it.
			return ctx.Err()
		}
		sel, release, selErr := w.selector.Select(ctx, model, tried...)
		if selErr != nil {
			return w.finishItem(ctx, item, models.ItemStatusFailed, nil,
				strPtr(selectFailureMessage(model, selErr, len(tried) > 0)))
		}
		tried = append(tried, sel.Backend.ID)

		resp, doErr, failover := w.callBackend(ctx, sel, release, item.Request)
		if doErr == nil {
			return w.finishItem(ctx, item, models.ItemStatusSucceeded, resp.Body, nil)
		}
		if failover && ctx.Err() == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return w.finishItem(ctx, item, models.ItemStatusFailed, nil, strPtr(doErr.Error()))
	}
}

// callBackend performs one backend attempt, always releasing the in-flight
// slot and always reporting a circuit outcome (mirroring the gateway's chat
// handler): success and permanent upstream errors report success (the
// backend answered), transient failures report failure, and a call ended by
// ctx reports canceled — verdict-free. The bool reports whether the caller
// should fail over (transient only).
func (w *Worker) callBackend(ctx context.Context, sel routing.Selection, release func(), body []byte) (*inference.Response, error, bool) {
	backend := sel.Backend.Name
	defer release()
	reported := false
	defer func() {
		if !reported {
			w.circuit.ReportCanceled(backend)
		}
	}()

	resp, err := w.inference.Do(ctx, sel.Backend, body)
	if err == nil {
		w.circuit.ReportSuccess(backend)
		reported = true
		return resp, nil, false
	}
	switch {
	case ctx.Err() != nil:
		w.circuit.ReportCanceled(backend)
		reported = true
		return nil, err, false
	case inference.IsTransient(err):
		w.circuit.ReportFailure(backend)
		reported = true
		return nil, err, true
	default:
		w.circuit.ReportSuccess(backend)
		reported = true
		return nil, err, false
	}
}

// finishItem durably writes the item's terminal result and counts it. An
// ErrNotFound means another delivery finished the item first (at-least-once
// overlap) — the stored result is authoritative and nothing is counted
// twice. Any other store error aborts the pass so the message is not acked.
func (w *Worker) finishItem(ctx context.Context, item models.BatchJobItem, status models.ItemStatus, response []byte, errMsg *string) error {
	err := w.store.UpdateItemTerminal(ctx, item.ID, status, response, errMsg)
	if errors.Is(err, jobs.ErrNotFound) {
		w.logger.Warn("item already terminal; keeping stored result",
			"item_id", item.ID, "attempted_status", string(status))
		return nil
	}
	if err != nil {
		w.metrics.WorkerFailuresTotal.Inc()
		w.logger.Error("terminal item update failed", "item_id", item.ID, "error", err)
		return err
	}
	w.metrics.BatchItemsProcessedTotal.WithLabelValues(string(status)).Inc()
	return nil
}

// reclaimOnce recovers messages stranded by crashed consumers: claim
// everything pending longer than ReclaimMinIdle and run each through the
// normal handler. Claimed-but-failed messages simply stay pending for the
// next tick.
func (w *Worker) reclaimOnce(ctx context.Context) {
	msgs, err := w.queue.Claim(ctx, w.cfg.ReclaimMinIdle)
	if err != nil {
		if ctx.Err() == nil {
			w.metrics.WorkerFailuresTotal.Inc()
			w.logger.Error("stream reclaim failed", "error", err)
		}
		return
	}
	for _, msg := range msgs {
		if ctx.Err() != nil {
			return
		}
		// XAUTOCLAIM reassigned msg to this consumer (resetting its idle clock,
		// harmless), but if the consume loop is still processing it, skip:
		// handleMessage would otherwise run a second concurrent pass over the
		// same job. handleMessage's own in-flight guard is the backstop.
		if w.isInFlight(msg.ID) {
			continue
		}
		if err := w.handleMessage(ctx, msg); err != nil && ctx.Err() == nil {
			w.logger.Error("reclaimed message failed; leaving pending",
				"message_id", msg.ID, "error", err)
		}
	}
}

// isInFlight reports whether this process is currently handling the delivery.
func (w *Worker) isInFlight(msgID string) bool {
	w.inflightMu.Lock()
	defer w.inflightMu.Unlock()
	_, ok := w.inflight[msgID]
	return ok
}

// drainOutboxOnce publishes pending outbox rows so a job committed to
// Postgres gets enqueued even if the API's original publish failed. A row is
// marked published only after Queue.Publish succeeded; a failed publish is
// recorded and the row stays pending for the next tick. A crash between
// publish and the mark yields a duplicate publish later — absorbed by the
// worker's per-item idempotency.
func (w *Worker) drainOutboxOnce(ctx context.Context) {
	rows, err := w.store.PendingOutbox(ctx, w.cfg.OutboxBatch)
	if err != nil {
		if ctx.Err() == nil {
			w.metrics.WorkerFailuresTotal.Inc()
			w.logger.Error("pending outbox read failed", "error", err)
		}
		return
	}
	for _, ob := range rows {
		if ctx.Err() != nil {
			return
		}
		if err := w.queue.Publish(ctx, ob.JobID.String()); err != nil {
			w.metrics.WorkerFailuresTotal.Inc()
			w.logger.Warn("outbox publish failed; row stays pending",
				"job_id", ob.JobID, "error", err)
			if merr := w.store.MarkOutboxFailedAttempt(ctx, ob.ID, err.Error()); merr != nil {
				w.logger.Error("outbox failed-attempt mark failed", "outbox_id", ob.ID, "error", merr)
			}
			continue
		}
		if err := w.store.MarkOutboxPublished(ctx, ob.ID); err != nil {
			w.logger.Warn("outbox published mark failed; duplicate publish possible",
				"outbox_id", ob.ID, "error", err)
		}
	}
}

// selectFailureMessage renders a routing failure as the item's stored error.
// Only a genuinely unknown model (no backend tried yet) blames the request;
// exhausted capacity or spent candidates are an availability problem.
func selectFailureMessage(model string, err error, failedOver bool) string {
	switch {
	case errors.Is(err, routing.ErrNoBackends) && !failedOver:
		return fmt.Sprintf("model %q is not served by any backend", model)
	case errors.Is(err, routing.ErrNoBackends), errors.Is(err, routing.ErrNoCapacity):
		return "no backend is currently available for this model"
	default:
		return fmt.Sprintf("routing failed: %v", err)
	}
}

// modelFromRequest extracts the model from a stored item request body.
func modelFromRequest(request []byte) string {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		return ""
	}
	return body.Model
}

func strPtr(s string) *string { return &s }
