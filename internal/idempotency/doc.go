// Package idempotency makes POSTs safe to retry: a repeated Idempotency-Key
// with the same body replays the first completed response, a reused key with
// a different body is a conflict, and a key whose first request is still
// running reports in-progress. Postgres (idempotency_records) is
// authoritative through the IdempotencyStore interface; Classify is the one
// shared source of the record semantics. Scope embeds tenant, API key,
// method, and route so keys never collide across them — Stage 6's batch
// endpoint reuses the same flow.
package idempotency
