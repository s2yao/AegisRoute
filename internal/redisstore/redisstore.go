package redisstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/example/aegisroute/internal/config"
)

// Connect builds the shared Redis client from configuration. It deliberately
// does not ping: connectivity is checked by the caller (via Ping) at the
// moment it matters, so constructing a client never blocks or fails just
// because Redis is momentarily unreachable.
func Connect(ctx context.Context, cfg *config.Config) (*redis.Client, error) {
	if cfg.RedisAddr == "" {
		return nil, errors.New("redisstore: connect: REDIS_ADDR is empty")
	}
	return redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
		DB:   cfg.RedisDB,
	}), nil
}

// Ping is the explicit Redis health check, used at startup and by readiness
// probes. Kept separate from Connect so callers control when a round trip to
// Redis actually happens.
func Ping(ctx context.Context, client *redis.Client) error {
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redisstore: ping: %w", err)
	}
	return nil
}

// Streams names the batch-job stream and its consumer group. Both values come
// from the environment (STREAM_KEY / STREAM_GROUP) rather than Go constants so
// deployments and tests can isolate streams without a rebuild.
type Streams struct {
	Key   string // STREAM_KEY
	Group string // STREAM_GROUP
}

// StreamsFromConfig copies the stream identifiers out of the loaded config.
func StreamsFromConfig(cfg *config.Config) Streams {
	return Streams{Key: cfg.StreamKey, Group: cfg.StreamGroup}
}

// ConsumerName derives the per-worker consumer name as "<hostname>-<pid>" at
// runtime — never a fixed env value — so multiple worker processes register
// as distinct consumers in the group and pending entries can be traced back
// to the process that claimed them. If the hostname is unavailable the
// "worker" fallback still yields distinct names via the pid suffix.
func ConsumerName() string {
	host, err := os.Hostname()
	if err != nil {
		host = "worker"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
