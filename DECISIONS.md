# AegisRoute — DECISIONS

Locked decisions for the whole project. Every stage obeys these; no
substitutes. Each entry: the decision, then a one-sentence rationale.

## Language, module, binaries

- **Go with `go 1.25` in `go.mod`, module `github.com/example/aegisroute`** — one pinned language target keeps 7 independent sessions consistent; the module path is placeholder-scoped and renameable later with `go mod edit -module`.
- **Exactly three binaries: `cmd/gateway-api` (also `-migrate` / `-seed` flags), `cmd/control-worker`, `cmd/mock-llm`** — one gateway artifact with three modes means migrations/seeding can never drift from the server code, and the surface area stays explainable.

## Locked libraries

- **Router: `github.com/go-chi/chi/v5`** — stdlib-compatible `net/http` router whose route patterns (`/api/v1/backends/{id}`) double as bounded-cardinality metric labels.
- **Postgres: `github.com/jackc/pgx/v5` + `pgxpool`, raw SQL, no ORM** — full control over every query and schema decision, which is exactly the part worth being able to explain in an interview.
- **Redis: `github.com/redis/go-redis/v9`** — the canonical client, with first-class Redis Streams support needed for the batch queue.
- **Migrations: `github.com/pressly/goose/v3` with `//go:embed`** — migrations ship inside the binary so `-migrate` needs no files on disk, and goose's advisory lock makes concurrent runs safe.
- **Metrics: `github.com/prometheus/client_golang`** — the standard Go Prometheus client.
- **UUIDs: `github.com/google/uuid`**; **tests: stdlib `testing` + `github.com/stretchr/testify`**; **Redis-in-tests: `github.com/alicebob/miniredis/v2`**.
- **Config: hand-rolled in `internal/config`, stdlib `os` only** — zero dependencies and fully ours to explain; the Makefile (not a dotenv library) puts `.env` values into the environment for host runs.
- **Logging: stdlib `log/slog`, JSON handler** — structured logs without a logging dependency.

## Testing philosophy (non-negotiable)

- **`go test ./...` passes with no Docker, no Postgres process, no Redis process — forever** — the feedback loop stays seconds-fast and CI-trivial in every stage.
- **Interface-first at every I/O boundary: consumer-declared repository interfaces + in-memory fakes; a `Queue` interface with Redis-Streams impl and in-memory fake; cache/rate-limit/idempotency tested against miniredis** — business logic is testable without infrastructure because the consumers own the contracts (miniredis verified to support `XADD`/`XREADGROUP`/`XACK`/`XPENDING`/`XCLAIM`/`XAUTOCLAIM`, so even the queue adapter is unit-testable).
- **Real-infra tests are `//go:build integration`, run via `make test-integration` only** — plain `go test ./...` never touches them.
- **Circuit breaker, routing selection, cache-key canonicalization, validation, API-key hashing, batch status machine = pure functions / state machines** — tested directly with no mocks at all.

## Security & error handling

- **API keys stored only as `HMAC-SHA256(APP_KEY_HASH_SECRET, key)`** — a leaked database row reveals no usable credential, while lookup stays a deterministic O(1) hash compare.
- **Every response carries `X-Request-ID`** (accept inbound, else generate a UUID) — one id correlates client reports, logs, and upstream calls.
- **Errors are always `{"error":{"code","message","request_id"}}`** — one shape means clients and tests never special-case error parsing; codes: `unauthorized, bad_request, not_found, conflict, rate_limited, unsupported_streaming, internal, upstream_unavailable`.
- **Never log Authorization/Cookie/Token/Secret/Password/API-Key/X-Admin-Token values; never log request/response bodies by default** — log only method, route pattern, status, duration, request_id, tenant/api-key IDs, backend name, coarse outcome; `internal/observability.Redact` is the single gate.
- **Credentials in query params are rejected with `400`** — query strings leak into logs, proxies, and browser history, so they are never an acceptable credential channel.
- **Auth routing table (fixed):** public: `/healthz`, `/readyz`, `/metrics`; bearer API-key: `/v1/models`, `/v1/chat/completions`, `/api/v1/batch-jobs*`; admin token: `/api/v1/backends*`, `/api/v1/routing-policies*`. Batch-jobs are tenant routes — do **not** classify all `/api/v1/*` as admin-only.

## Reliability semantics

- **Two independent retry layers that stack — do not conflate:** `RETRY_MAX_ATTEMPTS` retries the *upstream backend HTTP call* on transient failures (timeout, conn error, 502/503/504) inside one logical request; `WORKER_MAX_ITEM_ATTEMPTS` bounds how many times a *batch item* is re-attempted across stream redeliveries/claims before the DLQ. A single item can burn several HTTP retries inside one worker attempt.
- **`max_in_flight` is a per-process, per-backend semaphore** — documented as *not* global/distributed; distributed concurrency control is an explicit non-goal.

## Migrations vs seeding vs auto-migrate

- **Migrations = schema only** — no secrets, no key hashes, no seed rows in SQL migration files, ever, because migration files are permanent and secrets are not.
- **Seeding = one idempotent Go path (`internal/seed`)**, run via `-seed` or on startup when `AEGISROUTE_AUTO_SEED=true` — idempotent so re-running is always safe.
- **Auto-migrate/auto-seed on startup is a demo convenience, not a production pattern** — `.env.example` sets both `true` so local dev "just works", but a real deployment runs `-migrate` as a discrete deploy step (init job / CI) with `AEGISROUTE_AUTO_MIGRATE=false` so N replicas don't all migrate on boot; goose's advisory lock makes the race *safe*, but deliberate is better.

## Config validation modes

- **Three validators — `ValidateForMigrate` (only `DATABASE_URL`), `ValidateForSeed` (DB + `APP_KEY_HASH_SECRET` + `DEV_API_KEY` + both `SEED_BACKEND_*` URLs), `ValidateForServe` (full runtime config)** — one-off ops must not fail on unrelated runtime variables.
- **`APP_KEY_HASH_SECRET` must be ≥ 32 bytes in Seed/Serve validation** — a short HMAC secret weakens every stored key hash at once.
- **Uppercase env vars only; config fields `AutoMigrate`/`AutoSeed` load from `AEGISROUTE_AUTO_MIGRATE`/`AEGISROUTE_AUTO_SEED`.**

## Observability

- **`internal/metrics` owns one non-global `prometheus.Registry`; features increment via an injected `*Metrics`** — no global default registry means tests can build isolated instances and `New()` can never double-register.
- **Exact exported metric names (lowercase snake_case; Go fields stay PascalCase):**

| Go field | Exported name | Type / labels |
| --- | --- | --- |
| `HTTPRequestsTotal` | `aegisroute_http_requests_total` | counter; `route,method,status` |
| `HTTPRequestDurationSeconds` | `aegisroute_http_request_duration_seconds` | histogram; `route,method` |
| `BackendRequestsTotal` | `aegisroute_backend_requests_total` | counter; `backend,outcome` |
| `BackendRequestDurationSeconds` | `aegisroute_backend_request_duration_seconds` | histogram; `backend` |
| `CacheEventsTotal` | `aegisroute_cache_events_total` | counter; `result` (hit\|miss\|bypass) |
| `RateLimitedTotal` | `aegisroute_rate_limited_total` | counter |
| `BatchJobsCreatedTotal` | `aegisroute_batch_jobs_created_total` | counter |
| `BatchItemsProcessedTotal` | `aegisroute_batch_items_processed_total` | counter; `outcome` |
| `WorkerFailuresTotal` | `aegisroute_worker_failures_total` | counter |
| `CircuitBreakerState` | `aegisroute_circuit_breaker_state` | gauge; `backend` |

- **The `route` label is always the chi route pattern, never the raw path** — bounded label cardinality. HTTP response headers keep conventional `X-Capitalized` form; only metric names are lowercased.

## Gateway core (Stage 3)

- **Middleware chain order is fixed: recover → request-id → redacted logging → metrics → reject-query-credentials, then route-scoped auth** — recover is outermost so it catches panics in every other middleware; auth is applied per route group (chi `r.Group`) so it always runs after the shared chain. The Stage-3 log line carries exactly method, route pattern, status, duration, request_id — no bodies, no headers, no IDs.
- **The admin token is presented in the `X-Admin-Token` header, compared with `crypto/subtle.ConstantTimeCompare`; an empty configured token authorizes nobody** — a header (never a query param) keeps the token out of logs/proxies/history, and the empty-token guard defeats `ConstantTimeCompare("","")==1`.
- **`auth.KeyStore.GetByHash` returns `(*models.APIKey, error)` and `db.APIKeyRepo` was changed to match** — a nil key with `db.ErrNotFound` means "unknown key → 401" while any other error means "infra failure → 500", so a database outage never looks like an auth failure. BearerAuth imports `db` only for the `ErrNotFound` sentinel (acyclic; db never imports auth).
- **API consumer-declared interfaces live in `internal/api` (`BackendStore`, `PolicyStore`, `Pinger`) and readiness pings go through the tiny `Pinger` interface** — handlers and `/readyz` are unit-tested with in-memory fakes and no live Postgres/Redis; `cmd/gateway-api` adapts `db.Ping`/`redisstore.Ping` into `Pinger`.
- **Admin CRUD uses pointer-field request DTOs; PATCH DTOs omit immutable columns entirely** — pointers distinguish "omitted" from "zero", so create enforces required fields and patch touches only sent fields; a backend's `name`/`model_name`/`kind` and a policy's `name`/`model_name`/`strategy` cannot be rewritten because they are absent from the patch shape. Disable is always soft (`enabled=false`), never a hard delete.
- **Seeding converges the database to the declared demo state via `ON CONFLICT (name) DO UPDATE`** (`BackendRepo.Upsert`, `RoutingPolicyRepo.Upsert`) — re-running `-seed` (or `AEGISROUTE_AUTO_SEED` on boot) is always safe and always leaves the local stack in a known-good config; backend base URLs come from `SEED_BACKEND_*` config, never hardcoded, so host (localhost) and compose (service name) runs share one seed path.
- **`db.IsUniqueViolation` maps Postgres SQLSTATE 23505 to a 409 Conflict** on duplicate-name admin creates, reusing the `conflict` error code instead of surfacing a generic 500.

## Sync inference (Stage 4)

- **Transient = timeout, connection error, 502/503/504 — exactly; every other non-2xx (including 500) is permanent and never retried** — the retry layer's job is riding out infrastructure blips, and only that closed set is unambiguously a blip; a 500 is the backend answering "I'm broken", which retries won't fix.
- **A permanent upstream error is reported to the circuit breaker as a *success*** — the breaker measures reachability, not correctness: a backend returning 400 is alive and healthy, and counting its 4xxs as failures would let one malformed tenant request open the circuit for everyone.
- **One `routing.Breaker` instance is shared by the Selector (skip open circuits) and the chat handler (report outcomes)**, wired once in `cmd/gateway-api`; its state listener sets `aegisroute_circuit_breaker_state` with the fixed mapping 0=closed, 1=half-open, 2=open (`routing.CircuitStateGaugeValue`).
- **Half-open admits exactly one probe** (`Allow` reserves the slot; the outcome report frees it), and **outcomes reported while a circuit is already open are ignored** — stragglers from before the trip can neither close the circuit early nor extend the cooldown.
- **The Selector acquires the max_in_flight semaphore *before* consulting `Breaker.Allow`** — Allow consumes the single half-open probe slot, so it must only be spent by a candidate that already holds capacity to actually make the call.
- **`max_in_flight` semaphores are per-process and keyed by backend ID; a changed max_in_flight re-creates the semaphore** (in-flight holders drain into the old instance, briefly over-admitting) — distributed concurrency control stays an explicit non-goal.
- **Handler status mapping:** `ErrNoBackends` → 404 `not_found` (unknown model); `ErrNoCapacity` → 503 `upstream_unavailable`; transient inference failure → 503 `upstream_unavailable`; permanent inference failure → 502 `upstream_unavailable`; body over 1 MiB → 413 with code `bad_request` (no dedicated code in the fixed set).
- **Chat validation is strict at every level** (`DisallowUnknownFields`, so unknown *message* fields are also rejected, not just top-level); `temperature`/`max_tokens` are `*float64`/`*int` so omitted ≠ 0 (Stage 5 cache eligibility); `stop` normalizes to `[]string`; the forwarded body is re-marshalled canonical JSON — omitted optionals stay omitted and `stream` is never forwarded.
- **The inference_requests ledger write is best-effort** — a failed insert is logged and never turns a served completion into a client error; `request_hash` is the SHA-256 hex of the canonical forwarded body.
- **mock-llm's whole response is byte-deterministic per request body** — content derives from SHA-256 of the raw body and `created` is the fixed constant 1735689600, so identical inputs produce identical bytes (what makes Stage-5 caching observable in demos). Backend metrics count every attempt (`success|transient_error|permanent_error|canceled`), incremented inside `inference.Client` so handler and worker share instrumentation.
- **Caller-context cancellation is never a backend verdict** — a client disconnect surfaces from `inference.Client` as a non-transient error wrapping the context error (metrics outcome `canceled`), and the handler routes it to `Breaker.ReportCanceled`, which counts nothing and only returns a reserved half-open probe slot; otherwise N disconnects could open the circuit on a perfectly healthy backend, and a canceled probe could leave a backend unprobeable forever. A per-attempt timeout (parent context still alive) remains transient — that one *is* a backend verdict.
- **Chat strictness is case-SENSITIVE, enforced on the raw JSON key sets** (top level and inside each message) — encoding/json matches struct tags case-insensitively, so `"MODEL"`, `"Stream"`, or `"Role"` would silently bind (and even override the lowercase key); decoding to `map[string]json.RawMessage` first and checking exact keys closes that hole, which also protects Stage-5 cache-key canonicalization.
- **The inference_requests ledger write is asynchronous and off the request hot path** (`api.AsyncLedger`: a buffered queue drained by a bounded worker pool, each insert on its own `context.Background()` + 5s timeout) — a served completion never waits on Postgres, and a slow/degraded DB inflates no response latency. `Record` is fire-and-forget and non-blocking: when the queue is full it drops the row with a warning rather than stalling the handler (best-effort audit). Rows already queued are flushed on shutdown (`Close`, drain-bounded), and the ledger is closed before the pool so the workers can still write while draining.
- **The completion handler fails over within one request:** on a *transient* backend failure it re-selects, excluding backends already tried (`Selector.Select(ctx, model, exclude...)`), and calls the next healthy one — so a single request survives a dead backend instead of returning 503 until the circuit trips. Permanent errors (4xx) and cancellations do **not** fail over (a peer serving the same model rejects the same request; a client that left is gone). The whole operation — all failover attempts combined — is bounded by `config.InferenceBudget()` (`ServerWriteTimeout − 2s margin = 28s`) so failover across N backends can never overrun the socket write deadline.
- **`config.ServerWriteTimeout` (30s) is the single source of truth for the write deadline**, referenced by both `cmd/gateway-api`'s `http.Server` and `ValidateForServe`, which rejects at boot any config whose single-backend worst case (`BACKEND_TIMEOUT_MS × RETRY_MAX_ATTEMPTS + backoff`) exceeds the inference budget — so one healthy-but-slow backend can never blow the deadline mid-response, and the server and validator can't drift.
- **The circuit outcome report is panic-safe:** `callBackend` defers both the in-flight-slot `release()` and a "if not yet reported, `ReportCanceled`" cleanup, so a panic anywhere in the inference call frees the semaphore slot and releases the breaker's reserved half-open probe (verdict-free) before the recover middleware turns the panic into a 500 — a backend can never get stuck half-open un-probeable.
- **`inference.Client` caps the response body at `MaxResponseBytes` (default 10 MiB via `io.LimitReader`)** — a backend streaming an unbounded or maliciously huge body is rejected as a permanent error (not retried, so no OOM-retry loop) instead of being read into memory unbounded.

## Request-path reliability (Stage 5)

The full precedence order and its rationale live in `docs/design-decisions.md`
(the authoritative note). Locked decisions on top of it:

- **Cache and idempotency are separate mechanisms with separate keys** — the cache keys on `sha256(tenant/api-key scope ‖ canonical parsed body)` (sorted object keys, array order preserved); idempotency keys on `(scope=tenant+key+method+route, Idempotency-Key)` and hashes the exact raw request bytes. Different `Idempotency-Key`s therefore never block a genuine cache HIT for the same canonical body.
- **Omitted temperature normalizes to OpenAI's default 1.0 for cache eligibility, never Go's zero value** — omitted is NOT cacheable; explicit `0` is; the threshold is effective temperature ≤ 0.2 with `stream:false`. Only 2xx backend responses are stored.
- **`idempotency.Classify` is the single source of record semantics** (expired → absent; body mismatch → conflict regardless of state; completed → replay; pending → in-progress while locked, stale after) — the Postgres repo and every test fake call it, so store implementations cannot drift.
- **`Begin` is one atomic `INSERT … ON CONFLICT … DO UPDATE … WHERE` on the DB clock** — reclaiming expired/stale-pending rows and losing races in a single statement; the loser gets `ErrRecordActive` and folds back into replay/conflict via a re-lookup. Pending lock TTL = 2× `ServerWriteTimeout`, so a live request can never be reclaimed mid-flight. **A reclaim mints a fresh record id (`id = gen_random_uuid()`)** — without this, Postgres's `DO UPDATE` keeps the original PK, and a dead/lapsed owner's `Complete(oldID)` would find the reclaimed row still pending and overwrite the new owner's response with a stale one. The fresh id makes the old owner's `Complete`/`Release` a safe no-op (`id AND status='pending'` guard). The in-memory fakes and the integration test both assert this.
- **Every response after `Begin` resolves the record; the terminal action depends on status** — `< 500` (success, or a deterministic 4xx like unknown-model) → `Complete` (a same-key retry replays it); `>= 500` (retryable server/upstream failure) → `Release` (delete the record so the retry is a fresh attempt). Caching a transient 503 for the 24h TTL would lock out a client that correctly reuses its Idempotency-Key on retry — the opposite of retry-safety — so 5xx is never stored (the Stripe stance: only definitive outcomes are saved). `X-Request-ID` is never stored/replayed as a header; a replayed body keeps the original envelope's `request_id` field. The resolve runs on a detached context (client disconnects cannot strand a pending record).
- **The `Idempotency-Key` is capped at 255 characters** (400 before any store touch) — it is part of a unique index; an unbounded client value is an index-bloat vector.
- **No Redis fast path for idempotency in Stage 5** — Postgres is authoritative and sufficient; the spec's optional completed-record fast path can be added later behind the same `IdempotencyStore` interface without changing callers.
- **The rate limiter is a Redis fixed window whose INCR and PEXPIRE happen in one Lua invocation** — a counter without an expiry (the classic INCR-then-crash bug, which would rate-limit a key forever) is unrepresentable. One budget per API key shared by all bearer routes: `/v1/models` via `rateLimitMiddleware`, the chat handler inline at its precedence point (never both on one route — that would double-charge).
- **Fail-open vs fail-closed is deliberate per mechanism** — limiter and cache errors fail OPEN (degrade the feature, never availability); idempotency store errors fail CLOSED with 500 (guessing about replay correctness would break the exactly-once contract the client was promised).
- **Chat ledger rows always carry `cache_result` (hit|miss|bypass) from Stage 5 on; HIT rows have `backend_id` NULL** — a hit called no backend; the row's `request_hash` stays the canonical-body hash (cache-aligned), distinct from the idempotency raw-bytes hash.

## Docker / compose (applies from Stage 7; integration checkpoints earlier)

- **`docker compose` (v2, with a space), never `docker-compose`.**
- **No `platform: linux/amd64` pins** — all images used are multi-arch; Apple Silicon runs them natively on arm64.
- **Networking: service names inside compose (`postgres:5432`, `redis:6379`, `mock-llm-fast:8081`), `localhost` from the host** — compose env for the services uses service names; the host `.env` keeps `localhost`; do not "fix" one to match the other.

## Demo credentials (LOCAL-ONLY)

- API key `sg_dev_key_123` (stored only as its HMAC), admin token `dev_admin_token` — marked LOCAL-ONLY in `.env.example` and never valid anywhere but a local dev stack.
