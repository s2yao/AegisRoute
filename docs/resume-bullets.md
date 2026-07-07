# Resume / handoff bullets

Interview-ready descriptions of what AegisRoute is and the engineering decisions
worth talking about. Pick the ones that fit the conversation.

## One-liner

Built a Go LLM inference gateway / control plane (three services over Postgres +
Redis) that fronts OpenAI-compatible model backends with auth, policy-based
routing, circuit breaking, response caching, request idempotency, per-key rate
limiting, and a durable at-least-once batch pipeline — all covered by a
Docker-free test suite plus an integration tier and a one-command end-to-end
verification.

## Bullets

- Designed a **three-binary** control plane (`gateway-api`, `control-worker`,
  `mock-llm`) where migrations and seeding are modes of the gateway binary, so
  schema and server can never drift; deterministic fake backends make
  cache/idempotency/batch behavior observable in a demo.
- Implemented **policy-based backend routing** with priority ordering and a
  weighted tie-break, a per-process `max_in_flight` semaphore per backend, and a
  **circuit breaker** (closed → open → half-open, single probe) that skips
  failing backends; a request **fails over** across backends within its own
  deadline instead of returning an error.
- Built the **request-path reliability stack** with a load-bearing, test-pinned
  precedence: raw-body hashing → strict validation → idempotency check → rate
  limit → begin → cache → route/inference → cache store → resolve. Cache and
  idempotency are independent mechanisms with independent keys.
- Made **idempotency Postgres-authoritative** with a single atomic
  `INSERT … ON CONFLICT … WHERE` reclaim on the DB clock; a reclaim mints a fresh
  record id so a crashed/lapsed owner can't overwrite the reclaimer. Only
  definitive (`< 500`) outcomes are stored; retryable `5xx` releases the record
  so a same-key retry stays fresh (the Stripe stance).
- Chose **fail-open vs fail-closed per mechanism**: cache and rate limiting
  degrade open on a Redis outage (availability first); idempotency fails closed
  with a 500 (never guess about exactly-once). The rate limiter's `INCR`+`PEXPIRE`
  is one Lua invocation so a counter can never orphan its expiry.
- Built the **asynchronous batch path** with a **transactional outbox** (job +
  items + outbox row in one transaction, then one job-level publish; a failed
  publish leaves the outbox pending for a drain loop) over **Redis Streams**
  (consumer group, `XREADGROUP`, `XACK`, `XAUTOCLAIM`), a **bounded worker pool**,
  atomic item claims (`FOR UPDATE SKIP LOCKED`), a **DLQ** after a bounded item
  attempt count, and ack-only-after-durable-write so at-least-once delivery is
  idempotent per item.
- Hardened the worker against real distributed-systems edges found in review:
  a message for a deleted job is dead-lettered instead of redelivering forever;
  an in-flight guard stops the reclaim loop from double-processing (and
  prematurely exhausting) a long-running job in the same process; streams are
  trimmed (`MAXLEN ~`) so `XACK`-without-delete doesn't grow Redis unbounded.
- Kept the whole suite **Docker-free** via consumer-declared interfaces and
  in-memory fakes, with the Redis-Streams adapter unit-tested against
  **miniredis** and a real-infra tier behind `//go:build integration`; added a
  one-command **`make verify-e2e`** that stands up the full Compose stack, proves
  MISS→HIT and a batch to terminal, checks metrics, runs integration tests, and
  tears down — all with time-bounded waits.
- Instrumented everything with a fixed `aegisroute_*` Prometheus set on a
  non-global registry (no accidental double-registration, small deterministic
  `/metrics`), structured `slog` JSON logs with a single redaction gate, and an
  `X-Request-ID` on every response.

## What I would do next (see future-work.md)

Real OpenAI-compatible providers behind the same `Client`, SSE streaming, OIDC +
RBAC for the admin plane, consumer-group lag metrics, and — the big one —
distributed concurrency control to make `max_in_flight` and the breaker global
rather than per-process.
