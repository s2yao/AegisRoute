# Design decisions — request-path precedence (Stage 5)

This file documents the exact evaluation order on `POST /v1/chat/completions`
and why each step sits where it does. The order is load-bearing: several
Stage-5 tests pin it, and Stage 6's `POST /api/v1/batch-jobs` reuses the same
generic idempotency flow and rate limiter.

## The order

```
1. Global middleware (Stage 3, outermost first):
   recover → request-ID → logging → metrics → reject credential query params
2. Bearer auth (route group)                     → 401 unauthorized
3. Read the raw body ONCE, capped at 1 MiB       → 413 bad_request
4. Compute the idempotency request hash from the exact raw bytes
5. Parse + validate strictly                     → 400 / unsupported_streaming
6. Idempotency replay/conflict lookup (Check)    → replay stored response,
                                                   or 409 conflict
7. Rate limit — only if this is new work         → 429 rate_limited
8. Idempotency begin pending (Begin)             → 409 while another attempt
                                                   is pending
9. Cache lookup                                  → HIT: respond from cache
10. Route → inference (intra-request failover, bounded by the budget)
11. Cache store (only if eligible AND backend returned 2xx)
12. Persist inference_requests row (MISS/BYPASS rows carry the backend;
    HIT rows carry backend_id = NULL — written at step 9's HIT too)
13. Idempotency Complete with the response status/minimal headers/body —
    on EVERY path after step 8: success, cache HIT, or error envelope
```

`GET /v1/models` (no body, no idempotency) applies the same per-key limiter
as ordinary middleware after bearer auth. The chat route does **not** use
that middleware — its limit check is step 7 inside the handler, so both
routes share one budget but the chat route keeps replays free. Wrapping the
chat route in the middleware too would charge every request twice.

## Why this order

- **Credential query params are rejected before auth** so a credential in a
  URL is refused even when a valid header credential is also present — the
  URL variant must never work, anywhere.
- **Invalid requests never create pending records** (validate at step 5,
  Begin at step 8): a malformed request with an `Idempotency-Key` must not
  poison the key for the corrected retry.
- **Completed idempotency replay happens before rate limiting** (6 before 7):
  a replay costs no backend work, so it never consumes the caller's budget —
  retries of already-done work are free by design.
- **The rate limit runs before Begin** (7 before 8): a 429 leaves no record
  behind, so the client's retry after backoff starts clean.
- **Idempotency hashes the exact raw bytes** (step 4, before parsing): byte
  identity is the strictest, cheapest definition of "the same request" for
  retry safety, and it needs no canonicalization rules.
- **The cache keys on the canonicalized parsed body** (sorted object keys,
  original array order, tenant/api-key scope): semantically equal requests —
  different field order, whitespace — should share one entry. Cache and
  idempotency are **separate mechanisms with separate keys**: two E2E chat
  calls with different `Idempotency-Key`s do not replay (different keys) but
  the second is a genuine cache HIT (same canonical body).
- **A cache HIT still completes the opened idempotency record** (13 covers
  9): no path after Begin may leave a record pending, or same-key retries
  would 409 until the lock lapses.
- **Errors after Begin also Complete the record** — a retry with the same
  key replays the recorded error envelope (deterministic retries, the
  Stripe-style contract). The replayed *body* is the original envelope
  (including its `request_id` field, which identifies the request that
  actually executed); the `X-Request-ID` **header** is always the current
  request's — it is never stored and never replayed.
- **429 and 409 responses are never cached and never counted as cache
  events**: they end the request before the cache lookup.

## Failure-mode stances (fail open vs fail closed)

- **Rate limiter errors fail OPEN** (request allowed, warning logged): a
  Redis outage degrades rate limiting, never availability.
- **Cache errors fail OPEN as a MISS** (lookup) or a skipped store (write).
- **Idempotency store errors fail CLOSED with 500**: replay correctness
  cannot be guessed — serving new work when a completed record might exist
  would break the exactly-once illusion the client was promised.
- **A failed Complete is logged, not surfaced**: the response is served; the
  stranded pending record is reclaimed after its lock TTL (2× the server
  write deadline, so a live request is never reclaimed mid-flight).
