// Package ratelimit caps requests per API key with a Redis fixed-window
// counter (RATE_LIMIT_QPS per second). The increment and its expiry are one
// atomic Lua invocation, so a crash can never orphan a counter without a
// TTL. All bearer-auth routes share one budget per key; callers fail open on
// Redis errors so an outage degrades limiting, never availability.
package ratelimit
