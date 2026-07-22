package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces limiter counters in Redis.
const keyPrefix = "aegisroute:ratelimit:"

// window is a fixed-window counter: INCR the window's counter and — in the
// same atomic Lua invocation — arm its expiry when this call created it. A
// crash can therefore never leave a counter without an expiry (the failure
// mode of a separate INCR+EXPIRE pipeline), which would otherwise rate-limit
// a key forever on stale counts.
var window = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count`)

// Limiter caps requests per key (the gateway keys it by API-key ID) using a
// fixed window in Redis: at most limit requests per window duration. The
// window is anchored at the key's first request and resets when the counter
// expires. Per-process fairness only in the sense that all gateway replicas
// share the same Redis counters; the limit itself is global per key.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

// New builds a Limiter allowing limit requests per window per key
// (RATE_LIMIT_QPS with a one-second window in production).
func New(rdb *redis.Client, limit int, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window}
}

// Allow consumes one slot for key's current window and reports whether the
// request is within the limit. Errors (Redis down) are returned for the
// caller to decide on — the gateway fails open so a Redis outage degrades
// rate limiting, never availability.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, error) {
	count, err := window.Run(ctx, l.rdb, []string{keyPrefix + key}, l.window.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("ratelimit: %w", err)
	}
	return count <= int64(l.limit), nil
}
