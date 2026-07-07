// Package cache serves stored completion responses for identical
// low-temperature requests instead of calling a backend. Eligibility is
// strict: stream:false and an effective temperature ≤ 0.2, where an omitted
// temperature normalizes to OpenAI's default 1.0 (omitted is never cached;
// an explicit 0 is). Keys are sha256(scope ‖ canonical body): object keys
// sorted, array order preserved, and volatile headers structurally excluded.
// Entries live in Redis for CACHE_TTL_SECONDS and hold only the body plus
// safe content headers — replays never carry a stored X-Request-ID. The
// cache and idempotency are separate mechanisms with separate keys.
package cache
