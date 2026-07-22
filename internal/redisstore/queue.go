package redisstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Message is one logical batch-job hand-off read from the queue: the delivery
// id (needed to Ack or Claim this exact delivery) plus the job id payload.
// One message always represents one whole job, never one item — the worker
// fans out into items itself via Postgres claims.
type Message struct {
	// ID is the queue's delivery identifier (the Redis stream entry id).
	ID string
	// JobID is the batch_jobs.id the message tells the worker to process.
	JobID string
}

// Queue is the durable hand-off between the API (publish) and the worker
// (consume). Delivery is at-least-once: a consumer must tolerate seeing the
// same job id more than once. Consume never acks on its own — the consumer
// calls Ack itself, only after its durable Postgres update has committed.
// That ordering is the whole point: a crash between delivery and Ack leaves
// the message pending, and Claim later recovers it.
type Queue interface {
	// Publish appends one job-level message to the stream.
	Publish(ctx context.Context, jobID string) error
	// Consume delivers messages to handler until ctx ends. It does NOT ack:
	// a handler error (or a crash before Ack) leaves the message pending in
	// the consumer group for Claim to recover.
	Consume(ctx context.Context, handler func(ctx context.Context, msg Message) error) error
	// Ack marks one delivered message done. Call it only after the durable
	// Postgres update for that message has committed.
	Ack(ctx context.Context, msg Message) error
	// Claim transfers messages that have sat pending (delivered, never
	// acked) for at least minIdle to this consumer — recovery for messages
	// stranded by a crashed or wedged consumer.
	Claim(ctx context.Context, minIdle time.Duration) ([]Message, error)
	// PublishDLQ records a poisoned unit of work on the dead-letter stream
	// with a human-readable reason.
	PublishDLQ(ctx context.Context, msg Message, reason string) error
}

// Stream entry field names. The job stream carries only the job id; the DLQ
// stream adds the original delivery id and the reason.
const (
	fieldJobID     = "job_id"
	fieldMessageID = "message_id"
	fieldReason    = "reason"
)

// dlqSuffix names the dead-letter stream relative to the main stream key.
const dlqSuffix = ":dlq"

// maxStreamLen caps the batch stream with approximate trimming on every XADD.
// XACK removes a delivered entry from the consumer group's pending list but
// NOT from the stream itself, so without trimming the stream grows without
// bound as jobs are published. The cap is generous relative to any realistic
// backlog of un-consumed jobs, and "~" (approximate) trimming lets Redis trim
// at radix-node boundaries cheaply; a message is never trimmed while it is
// still un-acked in practice because the cap dwarfs the in-flight set.
const maxStreamLen = 100_000

// Defaults for the stream consumer. The block duration bounds one XREADGROUP
// wait so the consume loop can observe context cancellation between reads;
// the read count bounds one delivery batch.
const (
	defaultBlock     = 5 * time.Second
	defaultReadCount = 16
	// readErrorBackoff is the pause after an unexpected Redis error before
	// the consume loop retries, so a Redis outage doesn't spin the CPU.
	readErrorBackoff = time.Second
)

// StreamQueue is the Redis Streams implementation of Queue: XADD to publish,
// XREADGROUP as a per-instance consumer in one consumer group, XACK on Ack,
// XAUTOCLAIM on Claim, and XADD to "<stream>:dlq" for dead letters.
type StreamQueue struct {
	client   *redis.Client
	streams  Streams
	consumer string
	block    time.Duration
	count    int64
}

// StreamQueueOption customizes a StreamQueue at construction.
type StreamQueueOption func(*StreamQueue)

// WithBlock overrides how long one XREADGROUP call blocks waiting for
// messages. Tests use a short block so an empty stream never stalls them.
func WithBlock(d time.Duration) StreamQueueOption {
	return func(q *StreamQueue) { q.block = d }
}

// NewStreamQueue builds the Redis Streams queue over the shared client.
// consumer is this process's consumer-group member name (see ConsumerName).
func NewStreamQueue(client *redis.Client, streams Streams, consumer string, opts ...StreamQueueOption) *StreamQueue {
	q := &StreamQueue{
		client:   client,
		streams:  streams,
		consumer: consumer,
		block:    defaultBlock,
		count:    defaultReadCount,
	}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// Publish XADDs one job-level message. It does not touch the consumer group:
// the group is created by consumers from stream offset 0, so messages
// published before any worker ever ran are still delivered.
func (q *StreamQueue) Publish(ctx context.Context, jobID string) error {
	err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.streams.Key,
		MaxLen: maxStreamLen,
		Approx: true,
		Values: map[string]any{fieldJobID: jobID},
	}).Err()
	if err != nil {
		return fmt.Errorf("redisstore: publish job %s: %w", jobID, err)
	}
	return nil
}

// Consume reads the stream as this consumer until ctx ends, handing each
// message to handler. Handler errors are deliberately swallowed here: the
// un-acked message stays in the group's pending list, which is exactly the
// recovery contract (Claim picks it up later), and the handler owns its own
// logging.
func (q *StreamQueue) Consume(ctx context.Context, handler func(ctx context.Context, msg Message) error) error {
	if err := q.ensureGroup(ctx); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    q.streams.Group,
			Consumer: q.consumer,
			Streams:  []string{q.streams.Key, ">"},
			Count:    q.count,
			Block:    q.block,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue // block timeout with nothing to read
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Unexpected Redis failure: pause briefly and keep consuming so a
			// blip never kills the worker's consume loop.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(readErrorBackoff):
			}
			continue
		}
		for _, s := range streams {
			for _, xm := range s.Messages {
				_ = handler(ctx, messageFromX(xm))
			}
		}
	}
}

// Ack XACKs one delivery. Only call after the durable Postgres update.
func (q *StreamQueue) Ack(ctx context.Context, msg Message) error {
	if err := q.client.XAck(ctx, q.streams.Key, q.streams.Group, msg.ID).Err(); err != nil {
		return fmt.Errorf("redisstore: ack %s: %w", msg.ID, err)
	}
	return nil
}

// Claim XAUTOCLAIMs messages pending longer than minIdle over to this
// consumer, scanning from the start of the pending list each call. Claimed
// messages remain pending (idle reset) until acked, so a crash mid-recovery
// just means a later Claim finds them again.
func (q *StreamQueue) Claim(ctx context.Context, minIdle time.Duration) ([]Message, error) {
	if err := q.ensureGroup(ctx); err != nil {
		return nil, err
	}
	claimed, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   q.streams.Key,
		Group:    q.streams.Group,
		Consumer: q.consumer,
		MinIdle:  minIdle,
		Start:    "0",
		Count:    q.count,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redisstore: claim: %w", err)
	}
	out := make([]Message, 0, len(claimed))
	for _, xm := range claimed {
		out = append(out, messageFromX(xm))
	}
	return out, nil
}

// PublishDLQ appends the poisoned message and its reason to the dead-letter
// stream ("<stream>:dlq"). DLQ entries are terminal records for operators;
// nothing consumes them automatically.
func (q *StreamQueue) PublishDLQ(ctx context.Context, msg Message, reason string) error {
	err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.streams.Key + dlqSuffix,
		MaxLen: maxStreamLen,
		Approx: true,
		Values: map[string]any{
			fieldJobID:     msg.JobID,
			fieldMessageID: msg.ID,
			fieldReason:    reason,
		},
	}).Err()
	if err != nil {
		return fmt.Errorf("redisstore: publish dlq for job %s: %w", msg.JobID, err)
	}
	return nil
}

// ensureGroup creates the consumer group (and the stream, via MKSTREAM) if it
// does not exist yet. The group starts at offset 0, not $, so a worker booted
// after the API has already published jobs still consumes the backlog. An
// already-existing group (BUSYGROUP) is the normal case and not an error.
func (q *StreamQueue) ensureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.streams.Key, q.streams.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("redisstore: ensure group %s: %w", q.streams.Group, err)
	}
	return nil
}

// messageFromX converts a raw stream entry. A missing or non-string job_id
// yields an empty JobID; the worker treats that as a poison message and
// dead-letters it rather than failing the consume loop.
func messageFromX(xm redis.XMessage) Message {
	jobID, _ := xm.Values[fieldJobID].(string)
	return Message{ID: xm.ID, JobID: jobID}
}
