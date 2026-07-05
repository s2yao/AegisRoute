package api_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/models"
)

// countingStore records how many rows were inserted and can be primed to
// fail; Insert optionally blocks on gate to wedge the workers.
type countingStore struct {
	mu    sync.Mutex
	count int
	err   error
	gate  chan struct{} // when non-nil, Insert waits on it (or ctx)
}

func (s *countingStore) Insert(ctx context.Context, row models.InferenceRequest) (models.InferenceRequest, error) {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return models.InferenceRequest{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return models.InferenceRequest{}, s.err
	}
	s.count++
	return row, nil
}

func (s *countingStore) inserted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAsyncLedgerDrainsOnClose(t *testing.T) {
	store := &countingStore{}
	l := api.NewAsyncLedger(store, discardLogger(), 3, 64)

	const n = 40
	for range n {
		l.Record(models.InferenceRequest{Status: models.RequestStatusSucceeded})
	}
	l.Close() // must flush every accepted row before returning

	assert.Equal(t, n, store.inserted(), "Close drains all accepted rows")
}

func TestAsyncLedgerSwallowsInsertErrors(t *testing.T) {
	store := &countingStore{err: assert.AnError}
	l := api.NewAsyncLedger(store, discardLogger(), 2, 16)

	// Must not panic or block despite every insert failing.
	l.Record(models.InferenceRequest{Status: models.RequestStatusFailed})
	l.Record(models.InferenceRequest{Status: models.RequestStatusFailed})
	l.Close()

	assert.Equal(t, 0, store.inserted(), "failed inserts increment nothing, but are swallowed")
}

func TestAsyncLedgerRecordNeverBlocksWhenQueueFull(t *testing.T) {
	// One worker, tiny buffer, wedged store: Record must stay non-blocking and
	// drop overflow rather than stalling the caller (the request hot path).
	gate := make(chan struct{})
	store := &countingStore{gate: gate}
	l := api.NewAsyncLedger(store, discardLogger(), 1, 1)

	done := make(chan struct{})
	go func() {
		for range 200 {
			l.Record(models.InferenceRequest{Status: models.RequestStatusSucceeded})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked while the ledger queue was full")
	}

	close(gate) // let the wedged/queued inserts proceed
	l.Close()
	require.LessOrEqual(t, store.inserted(), 200, "no row is inserted more than once")
}
