package redisstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/redisstore"
)

func TestMemQueue_PublishConsumeAck(t *testing.T) {
	q := redisstore.NewMemQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, q.Publish(ctx, "job-1"))
	assert.Equal(t, 1, q.PublishCount())

	got := make(chan redisstore.Message, 1)
	go func() {
		_ = q.Consume(ctx, func(_ context.Context, msg redisstore.Message) error {
			got <- msg
			return nil
		})
	}()

	select {
	case msg := <-got:
		assert.Equal(t, "job-1", msg.JobID)
		require.NoError(t, q.Ack(ctx, msg))
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// After ack the message is neither pending nor in flight.
	assert.Eventually(t, func() bool { return q.InFlightCount() == 0 },
		time.Second, 5*time.Millisecond)
	assert.Len(t, q.Acked(), 1)
}

func TestMemQueue_UnackedStaysInFlightAndClaimRecovers(t *testing.T) {
	// Mirrors the stream adapter's at-least-once contract: a handler error
	// leaves the message in flight; Claim recovers it once idle.
	q := redisstore.NewMemQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, q.Publish(ctx, "job-x"))

	delivered := make(chan struct{}, 1)
	go func() {
		_ = q.Consume(ctx, func(_ context.Context, _ redisstore.Message) error {
			select {
			case delivered <- struct{}{}:
			default:
			}
			return errors.New("no ack")
		})
	}()
	<-delivered
	cancel()

	// Idle-zero claim returns the in-flight message; a large minIdle does not.
	none, err := q.Claim(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Empty(t, none)

	claimed, err := q.Claim(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "job-x", claimed[0].JobID)
}

func TestMemQueue_PublishErrInjection(t *testing.T) {
	q := redisstore.NewMemQueue()
	q.SetPublishErr(errors.New("redis down"))

	err := q.Publish(context.Background(), "job-1")
	require.Error(t, err)
	assert.Equal(t, 0, q.PublishCount())

	q.SetPublishErr(nil)
	require.NoError(t, q.Publish(context.Background(), "job-1"))
	assert.Equal(t, 1, q.PublishCount())
}

func TestMemQueue_PublishDLQ(t *testing.T) {
	q := redisstore.NewMemQueue()
	msg := redisstore.Message{ID: "mem-1", JobID: "job-poison"}
	require.NoError(t, q.PublishDLQ(context.Background(), msg, "exhausted"))

	dlq := q.DLQ()
	require.Len(t, dlq, 1)
	assert.Equal(t, "job-poison", dlq[0].Msg.JobID)
	assert.Equal(t, "exhausted", dlq[0].Reason)
}
