package redisstore_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/redisstore"
)

func TestConnect(t *testing.T) {
	t.Run("succeeds against miniredis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		cfg := &config.Config{RedisAddr: mr.Addr()}

		client, err := redisstore.Connect(context.Background(), cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })

		require.NoError(t, redisstore.Ping(context.Background(), client))
	})

	t.Run("empty RedisAddr errors", func(t *testing.T) {
		client, err := redisstore.Connect(context.Background(), &config.Config{})
		require.Error(t, err)
		assert.Nil(t, client)
		assert.Contains(t, err.Error(), "REDIS_ADDR")
	})
}

func TestPingFailsOnceServerIsGone(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := &config.Config{RedisAddr: mr.Addr()}

	client, err := redisstore.Connect(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	mr.Close()

	err = redisstore.Ping(context.Background(), client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redisstore: ping:")
}

func TestStreamsFromConfig(t *testing.T) {
	cfg := &config.Config{
		StreamKey:   "custom:stream",
		StreamGroup: "custom-group",
	}

	s := redisstore.StreamsFromConfig(cfg)
	assert.Equal(t, "custom:stream", s.Key)
	assert.Equal(t, "custom-group", s.Group)
}

func TestConsumerName(t *testing.T) {
	name := redisstore.ConsumerName()

	require.NotEmpty(t, name)
	assert.Contains(t, name, strconv.Itoa(os.Getpid()),
		"consumer name must embed the pid so workers on one host stay distinct")
	assert.Equal(t, name, redisstore.ConsumerName(),
		"consumer name must be stable within a process")
}
