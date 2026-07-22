package redisstore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DLQEntry is one dead-lettered message recorded by MemQueue, kept for test
// assertions.
type DLQEntry struct {
	Msg    Message
	Reason string
}

// MemQueue is the in-memory Queue used by Docker-free tests. It mirrors the
// Redis Streams semantics that matter to correctness: delivery moves a
// message to an in-flight (pending) set rather than removing it, Ack is the
// only way out of that set, a handler error leaves the message in-flight,
// and Claim returns in-flight messages whose delivery is at least minIdle
// old (resetting their idle clock, like XAUTOCLAIM). Safe for concurrent use.
type MemQueue struct {
	mu         sync.Mutex
	nextID     int
	pending    []Message
	inflight   map[string]inflightMsg
	dlq        []DLQEntry
	published  []string
	acked      []Message
	publishErr error
}

type inflightMsg struct {
	msg         Message
	deliveredAt time.Time
}

// NewMemQueue returns an empty in-memory queue.
func NewMemQueue() *MemQueue {
	return &MemQueue{inflight: map[string]inflightMsg{}}
}

// SetPublishErr makes every subsequent Publish fail with err until cleared
// with SetPublishErr(nil) — how tests exercise the outbox-stays-pending path.
func (q *MemQueue) SetPublishErr(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.publishErr = err
}

// Publish appends one job-level message, or fails with the injected error.
func (q *MemQueue) Publish(_ context.Context, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.publishErr != nil {
		return q.publishErr
	}
	q.nextID++
	q.pending = append(q.pending, Message{ID: fmt.Sprintf("mem-%d", q.nextID), JobID: jobID})
	q.published = append(q.published, jobID)
	return nil
}

// Consume delivers pending messages to handler until ctx ends. Each delivery
// moves the message to the in-flight set first, so a handler error (no Ack)
// leaves it recoverable via Claim — the same contract as the stream adapter.
func (q *MemQueue) Consume(ctx context.Context, handler func(ctx context.Context, msg Message) error) error {
	for {
		msg, ok := q.takeOne()
		if ok {
			_ = handler(ctx, msg)
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// takeOne pops the oldest pending message into the in-flight set.
func (q *MemQueue) takeOne() (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Message{}, false
	}
	msg := q.pending[0]
	q.pending = q.pending[1:]
	q.inflight[msg.ID] = inflightMsg{msg: msg, deliveredAt: time.Now()}
	return msg, true
}

// Ack removes a delivered message from the in-flight set. Acking a message
// that is not in flight is a harmless no-op (mirrors XACK on an unknown id).
func (q *MemQueue) Ack(_ context.Context, msg Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, msg.ID)
	q.acked = append(q.acked, msg)
	return nil
}

// Claim returns in-flight messages idle for at least minIdle, resetting each
// one's idle clock like XAUTOCLAIM does. Claimed messages stay in flight
// until acked.
func (q *MemQueue) Claim(_ context.Context, minIdle time.Duration) ([]Message, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	var out []Message
	for id, entry := range q.inflight {
		if now.Sub(entry.deliveredAt) >= minIdle {
			out = append(out, entry.msg)
			q.inflight[id] = inflightMsg{msg: entry.msg, deliveredAt: now}
		}
	}
	return out, nil
}

// PublishDLQ records the dead-lettered message for later inspection.
func (q *MemQueue) PublishDLQ(_ context.Context, msg Message, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dlq = append(q.dlq, DLQEntry{Msg: msg, Reason: reason})
	return nil
}

// PublishCount reports how many Publish calls succeeded.
func (q *MemQueue) PublishCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.published)
}

// Published returns the job ids published so far, in order.
func (q *MemQueue) Published() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.published...)
}

// DLQ returns the dead-letter entries recorded so far, in order.
func (q *MemQueue) DLQ() []DLQEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]DLQEntry(nil), q.dlq...)
}

// Acked returns the acked messages so far, in order.
func (q *MemQueue) Acked() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Message(nil), q.acked...)
}

// InFlightCount reports how many delivered messages are awaiting Ack.
func (q *MemQueue) InFlightCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inflight)
}

// PendingCount reports how many published messages have not been delivered.
func (q *MemQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
