package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLimiter(t *testing.T, limit int) (*Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, limit, time.Second), mr
}

func TestAllowUnderLimit(t *testing.T) {
	l, _ := newTestLimiter(t, 3)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		ok, err := l.Allow(ctx, "key-1")
		require.NoError(t, err)
		assert.Truef(t, ok, "request %d of 3 is within the limit", i)
	}
}

func TestAllowOverLimit(t *testing.T) {
	l, _ := newTestLimiter(t, 2)
	ctx := context.Background()
	for range 2 {
		ok, err := l.Allow(ctx, "key-1")
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := l.Allow(ctx, "key-1")
	require.NoError(t, err)
	assert.False(t, ok, "the third request in the window exceeds a limit of 2")
}

func TestWindowRefillsAfterTimeAdvance(t *testing.T) {
	l, mr := newTestLimiter(t, 1)
	ctx := context.Background()

	ok, err := l.Allow(ctx, "key-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = l.Allow(ctx, "key-1")
	require.NoError(t, err)
	require.False(t, ok, "window exhausted")

	mr.FastForward(time.Second + time.Millisecond)

	ok, err = l.Allow(ctx, "key-1")
	require.NoError(t, err)
	assert.True(t, ok, "the window's counter expires and the budget refills")
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, 1)
	ctx := context.Background()

	ok, err := l.Allow(ctx, "key-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = l.Allow(ctx, "key-1")
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = l.Allow(ctx, "key-2")
	require.NoError(t, err)
	assert.True(t, ok, "one key exhausting its budget must not affect another")
}

func TestAllowSurfacesRedisErrors(t *testing.T) {
	l, mr := newTestLimiter(t, 1)
	mr.Close()
	_, err := l.Allow(context.Background(), "key-1")
	assert.Error(t, err, "a down Redis surfaces as an error; the caller fails open")
}

func TestCounterCarriesExpiry(t *testing.T) {
	// The regression this package's Lua script exists to prevent: a counter
	// that INCRs without an expiry would rate-limit the key forever.
	l, mr := newTestLimiter(t, 1)
	_, err := l.Allow(context.Background(), "key-1")
	require.NoError(t, err)
	ttl := mr.TTL(keyPrefix + "key-1")
	assert.Greater(t, ttl, time.Duration(0), "the window counter must always carry an expiry")
}
