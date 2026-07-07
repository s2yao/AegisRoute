package redisstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/redisstore"
)

// newStreamQueue builds a StreamQueue over a fresh miniredis with a short
// block so an empty stream never stalls a test.
func newStreamQueue(t *testing.T) (*redisstore.StreamQueue, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	streams := redisstore.Streams{Key: "aegisroute:test", Group: "workers"}
	q := redisstore.NewStreamQueue(client, streams, "consumer-1", redisstore.WithBlock(50*time.Millisecond))
	return q, mr
}

func TestStreamQueue_PublishConsumeAck(t *testing.T) {
	q, _ := newStreamQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, q.Publish(ctx, "job-123"))

	got := make(chan redisstore.Message, 1)
	var consumeErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		consumeErr = q.Consume(ctx, func(_ context.Context, msg redisstore.Message) error {
			got <- msg
			return nil
		})
	}()

	select {
	case msg := <-got:
		assert.Equal(t, "job-123", msg.JobID)
		assert.NotEmpty(t, msg.ID, "delivery id is needed to ack")
		require.NoError(t, q.Ack(ctx, msg))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}

	cancel()
	wg.Wait()
	assert.ErrorIs(t, consumeErr, context.Canceled)
}

func TestStreamQueue_ConsumeDoesNotAutoAck_ClaimRecoversStranded(t *testing.T) {
	// The whole point of at-least-once: a delivered-but-unacked message must
	// stay pending and be recoverable by Claim after it has sat idle.
	q, mr := newStreamQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// miniredis's stream pending-entry idle time is measured against its
	// settable clock (FastForward only advances TTLs, not stream PEL times),
	// so pin a base time and later advance it to age the pending entry.
	base := time.Now()
	mr.SetTime(base)

	require.NoError(t, q.Publish(ctx, "job-stranded"))

	// Deliver once via Consume, but never ack (simulating a consumer that
	// crashed after delivery). Stop consuming as soon as we have it.
	delivered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = q.Consume(ctx, func(_ context.Context, msg redisstore.Message) error {
			assert.Equal(t, "job-stranded", msg.JobID)
			select {
			case delivered <- struct{}{}:
			default:
			}
			return errors.New("simulated handler failure: no ack")
		})
	}()
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("message never delivered")
	}
	cancel()
	wg.Wait()

	// Nothing has acked, so a claim before the idle window elapses finds
	// nothing yet.
	claimed, err := q.Claim(context.Background(), time.Minute)
	require.NoError(t, err)
	assert.Empty(t, claimed, "message has not been idle long enough to reclaim")

	// Advance miniredis's clock past the idle threshold; now the stranded
	// message is reclaimable.
	mr.SetTime(base.Add(2 * time.Minute))
	claimed, err = q.Claim(context.Background(), time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "job-stranded", claimed[0].JobID)

	// A reclaimed message can then be acked to clear it.
	require.NoError(t, q.Ack(context.Background(), claimed[0]))
}

func TestStreamQueue_PublishDLQ(t *testing.T) {
	q, mr := newStreamQueue(t)
	ctx := context.Background()

	msg := redisstore.Message{ID: "1-0", JobID: "job-poison"}
	require.NoError(t, q.PublishDLQ(ctx, msg, "unparseable job_id"))

	// The dead-letter stream is the main key plus ":dlq".
	assert.True(t, mr.Exists("aegisroute:test:dlq"),
		"DLQ entry must land on the <stream>:dlq stream")
	entries, err := mr.Stream("aegisroute:test:dlq")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	fields := fieldMap(entries[0].Values)
	assert.Equal(t, "job-poison", fields["job_id"])
	assert.Equal(t, "unparseable job_id", fields["reason"])
	assert.Equal(t, "1-0", fields["message_id"])
}

func TestStreamQueue_ConsumeSeesBacklogPublishedBeforeGroupExisted(t *testing.T) {
	// The consumer group is created at offset 0, so a job published before
	// any worker booted is still delivered — a real ordering in this system,
	// where the API can publish long before the worker starts.
	q, _ := newStreamQueue(t)
	require.NoError(t, q.Publish(context.Background(), "job-early"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan redisstore.Message, 1)
	go func() {
		_ = q.Consume(ctx, func(_ context.Context, msg redisstore.Message) error {
			got <- msg
			return nil
		})
	}()
	select {
	case msg := <-got:
		assert.Equal(t, "job-early", msg.JobID)
	case <-time.After(2 * time.Second):
		t.Fatal("backlog message was not delivered")
	}
}

// fieldMap converts miniredis's flat [field, value, field, value, ...] entry
// values into a map.
func fieldMap(values []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(values); i += 2 {
		out[values[i]] = values[i+1]
	}
	return out
}
