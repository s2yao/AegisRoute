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

## Docker / compose (applies from Stage 7; integration checkpoints earlier)

- **`docker compose` (v2, with a space), never `docker-compose`.**
- **No `platform: linux/amd64` pins** — all images used are multi-arch; Apple Silicon runs them natively on arm64.
- **Networking: service names inside compose (`postgres:5432`, `redis:6379`, `mock-llm-fast:8081`), `localhost` from the host** — compose env for the services uses service names; the host `.env` keeps `localhost`; do not "fix" one to match the other.

## Demo credentials (LOCAL-ONLY)

- API key `sg_dev_key_123` (stored only as its HMAC), admin token `dev_admin_token` — marked LOCAL-ONLY in `.env.example` and never valid anywhere but a local dev stack.
