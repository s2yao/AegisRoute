# Failure modes

How AegisRoute behaves when things break. The guiding rule: **fail open** for
features that degrade gracefully (cache, rate limit) and **fail closed** for the
one guarantee we cannot fake (idempotent exactly-once semantics). Every stance
below is deliberate and tested.

## Backend failures (sync path)

| Situation | Behavior |
| --- | --- |
| A backend times out or returns 502/503/504 | Classified **transient**; retried per `RETRY_MAX_ATTEMPTS` with full-jitter backoff, then the request **fails over** to another healthy backend (excluding those already tried), bounded by the inference budget (`ServerWriteTimeout − 2s`). |
| A backend returns 400/401/403/404 (or any other non-2xx) | Classified **permanent**; never retried and **not** counted against the circuit breaker — the backend answered, so it is alive. Surfaced as `502`. |
| A backend keeps failing transiently | After `CB_FAILURE_THRESHOLD` consecutive transient failures its circuit **opens**; the selector skips it until `CB_COOLDOWN_MS` elapses, then admits exactly one half-open probe. |
| Every backend for the model is open/full | `503 upstream_unavailable`. Unknown model (no backend serves it) → `404`. |
| The client disconnects mid-call | Reported as **canceled** — verdict-free: it never trips the breaker and never pollutes error metrics; a reserved half-open probe is returned so the backend stays probeable. |
| A backend streams an unbounded body | Capped at `MaxResponseBytes` (10 MiB) and rejected as a permanent error — no OOM, no retry loop. |

## Redis failures (fail open vs fail closed)

| Mechanism | On Redis error |
| --- | --- |
| Response cache | **Fail open** — a lookup error is treated as a MISS; a store error is skipped and logged. Availability is never sacrificed for a cache. |
| Rate limiter | **Fail open** — the request is allowed with a warning. A rate limiter outage must not become an availability outage. The window's `INCR`+`PEXPIRE` run in one Lua invocation, so a counter can never be left without an expiry. |
| Idempotency store lock | **Fail closed** — a store error returns `500`. Guessing whether a completed record exists could serve new work twice and break the exactly-once promise. |

## Postgres failures

- **Idempotency** is Postgres-authoritative. `Begin` is one atomic
  `INSERT … ON CONFLICT … WHERE` on the DB clock; a lost race yields
  `ErrRecordActive` and folds back into replay/conflict. A reclaim mints a fresh
  record id (`gen_random_uuid()`), so a crashed or lapsed owner's `Complete` /
  `Release` becomes a safe no-op and can never overwrite the reclaimer.
- **The audit ledger write is best-effort and asynchronous** (a bounded worker
  pool off the hot path): a slow or failing Postgres never adds latency to, or
  fails, a served completion; a full queue drops the row with a warning.
- **A failed `Complete`/`Release`** is logged, not surfaced — the response is
  still served, and a stranded pending record is reclaimed after its lock TTL
  (2× the server write deadline, so a live request is never reclaimed mid-flight).

## Batch / worker failures (async path)

| Situation | Behavior |
| --- | --- |
| The API publish fails after the transaction commits | The job is **not lost**: its outbox row stays `pending` and the worker's outbox-drain loop republishes it (`PendingOutbox → Publish → MarkOutboxPublished`). |
| A worker crashes mid-item | The message is never acked (ack happens only after the durable Postgres update + status recompute). On redelivery, an item left `running` is requeued and retried; already-terminal items are never re-claimed, so redelivery is idempotent. |
| A message is stranded by a dead consumer | The periodic `XAUTOCLAIM` reclaim loop transfers messages idle beyond the threshold to a live consumer. A message the local process is still handling is skipped (in-flight guard) so it is never double-processed. |
| An item keeps failing across redeliveries | Bounded by `WORKER_MAX_ITEM_ATTEMPTS`: once exhausted, the item is failed terminally with an explanatory error and dead-lettered to the `:dlq` stream; the rest of the batch continues. |
| A message references a job that no longer exists | It is dead-lettered and acked (dropped) rather than redelivered forever — the job was deleted (e.g. its tenant was removed, cascading). |
| An unparseable job id arrives on the stream | Dead-lettered and acked. |

Note the two independent retry layers, which stack and must not be conflated:
`RETRY_MAX_ATTEMPTS` retries one upstream HTTP call inside a single logical
request; `WORKER_MAX_ITEM_ATTEMPTS` bounds how many stream redeliveries/claims a
batch item survives before the DLQ.

## Config & startup failures

- Config validation is per run mode and fails loudly at boot, naming the
  offending variable (never its value, so secrets can't leak through an error).
  `ValidateForServe` also rejects a retry/timeout budget that cannot fit the
  server write deadline, so one healthy-but-slow backend can never blow the
  socket deadline mid-response.
- Migrations are idempotent (goose advisory lock); re-running `-migrate` is a
  clean no-op.
