# Code Graph

Navigation map for agents. Compact by design — it points you at the smallest
relevant file set, it is not a spec. Deeper prose already exists and is
authoritative where it overlaps: `docs/REPO_MAP.md` (architecture),
`DECISIONS.md` (locked decisions), `docs/design-decisions.md` (Stage-5
request precedence), `PROJECT_STATE.md` / `TODO.md` / `docs/STAGE_STATUS.md`
(current stage). Verify claims against code before acting.

## Repository purpose

AegisRoute is a Go LLM inference gateway / control plane. `gateway-api` sits
in front of one or more OpenAI-compatible model backends and adds control-plane
concerns: bearer/admin auth, backend routing, retry + circuit breaking,
response cache, idempotency, per-key rate limiting, an audit ledger, and
Prometheus metrics. Model backends are deterministic fakes (`mock-llm`) on
purpose — the value is the control plane, not chatbot quality. Batch jobs and
the `control-worker` are planned but not yet implemented (Stage 6).
Source: `PROJECT_STATE.md`, `go.mod`, `internal/api/router.go`.

## Languages and tooling

- Language: Go (`go 1.25.7`), module `github.com/example/aegisroute` (`go.mod`).
- HTTP router: `github.com/go-chi/chi/v5`.
- Postgres: `github.com/jackc/pgx/v5` (+ `pgxpool`), raw SQL, no ORM.
- Migrations: `github.com/pressly/goose/v3`, embedded via `//go:embed`.
- Redis: `github.com/redis/go-redis/v9`.
- Metrics: `github.com/prometheus/client_golang`.
- UUID: `github.com/google/uuid`.
- Tests: stdlib `testing` + `github.com/stretchr/testify`; Redis-in-tests via
  `github.com/alicebob/miniredis/v2`.
- Config/logging: hand-rolled `internal/config` (stdlib `os` only) + stdlib
  `log/slog` JSON.
- External runtime deps: PostgreSQL, Redis, and two `mock-llm` HTTP backends.
  Prometheus scrapes `/metrics`. Docker/Compose assets are Stage 7 and not yet
  present (`deploy/docker/` is an empty `.gitkeep`).
Source: `go.mod`, `Makefile`, `internal/*`, `DECISIONS.md`.

## Entrypoints

- `cmd/gateway-api/main.go` — **the HTTP API server (:8080).** Three modes by
  flag: `-migrate` applies embedded migrations and exits; `-seed` inserts demo
  data and exits; no flag = serve. Startup: `go run ./cmd/gateway-api`
  (serve mode needs `DATABASE_URL` + `REDIS_ADDR`; `make migrate-up` and
  `make seed-dev` run the flag modes). Downstream: `internal/api` (router +
  `Deps`), `internal/db`, `internal/redisstore`, `internal/routing`,
  `internal/inference`, `internal/cache`, `internal/idempotency`,
  `internal/ratelimit`, `internal/seed`, `internal/config`, `internal/metrics`,
  `internal/observability`.
- `cmd/mock-llm/main.go` — **deterministic fake OpenAI backend.** Serves
  `POST /v1/chat/completions` (content derived from a hash of the request →
  identical input yields identical output) and `GET /healthz`; env `MOCK_*`
  (port, latency, jitter, failure rate, backend/model name). Startup:
  `go run ./cmd/mock-llm`. Downstream: none (standalone).
- `cmd/control-worker/` — **Stage 6, not implemented.** Only a `.gitkeep`
  exists. Intended role: consume batch jobs from a Redis Stream with a bounded
  pool. Startup command: `unknown / needs verification`.
Source: `cmd/gateway-api/main.go`, `cmd/mock-llm/main.go`, `Makefile`,
`git ls-files cmd/control-worker/*`.

## Core modules

Grouped by role. `internal/jobs` and `internal/worker` are scaffold-only
(`doc.go`, no logic — Stage 6).

- `internal/api` — chi router, middleware chain, and all handlers (health,
  `/v1/models`, `/v1/chat/completions`, admin CRUD). Declares its own
  consumer-side interfaces in `router.go` (`ChatSelector`, `InferenceDoer`,
  `CircuitReporter`, `LedgerRecorder`, `ResponseCache`, `RateLimiter`,
  `IdempotencyGate`, `BackendStore`, `PolicyStore`, `Pinger`) so it never
  imports `pgx`/concrete impls. Key files: `router.go`, `chat.go` (the
  precedence-critical completion handler), `middleware.go`, `admin.go`,
  `models_handler.go`, `health.go`, `ledger.go` (async audit writer).
  Depends on: nearly every other `internal/*`. Depended on by:
  `cmd/gateway-api`.
- `internal/config` — env-var config + three validators
  (`ValidateForMigrate/Seed/Serve`), `ServerWriteTimeout`, `InferenceBudget()`.
  Depended on by: all binaries and most packages.
- `internal/httperror` — the one error envelope (`{"error":{code,message,
  request_id}}`) + `Write()` + the closed code set. Depended on by:
  `internal/api`, `internal/auth`.
- `internal/observability` — `slog` JSON logger, request-id context helpers,
  `Redact` (single secret-logging gate).
- `internal/metrics` — one non-global `prometheus.Registry`; all `aegisroute_*`
  collectors (`metrics.go`). Depended on by: `internal/api`,
  `internal/inference`.
- `internal/models` — shared domain structs + typed enums with `Parse*`/
  `IsValid` mirroring schema CHECK constraints. Depended on by: `db`, `api`,
  `routing`, `inference`, `cache`, `idempotency`, `seed`.
- `internal/db` — pgx pool, embedded goose migrations, and pgx repositories
  (`apikey_repo`, `backend_repo`, `routingpolicy_repo`, `tenant_repo`,
  `inference_repo`, `idempotency_repo`). `errors.go` maps `pgx.ErrNoRows`→
  `ErrNotFound` and unique violations. See `internal/db/CLAUDE.md`.
- `internal/redisstore` — shared go-redis client + stream identifiers (stream
  helpers land in Stage 6).
- `internal/auth` — `BearerAuth` (HMAC-SHA256 key lookup) + `AdminAuth`
  (constant-time token compare) middleware; `Principal` context helpers.
- `internal/seed` — idempotent demo-data seeder (`Run`).
- `internal/inference` — `Client.Do(ctx, backend, body)`: one outbound backend
  call with per-attempt timeout, transient-only retry with full-jitter backoff,
  response size cap, typed transient/permanent/canceled errors. Shared by the
  chat handler (and later the worker).
- `internal/routing` — `Selector.Select` (backend choice, policy fallback,
  per-process `max_in_flight` semaphores, priority + weighted tie-break,
  intra-request-failover exclusion) and `Breaker` (circuit breaker state
  machine). See `internal/routing/CLAUDE.md`.
- `internal/cache` — response cache: `Eligible`, `CanonicalBody`, `Key`
  (`sha256(scope ‖ canonical body)`), Redis Get/Put.
- `internal/idempotency` — `Classify` (single record-semantics source),
  `IdempotencyStore` (satisfied by `db.IdempotencyRepo`), `Coordinator`
  (Check/Begin/Complete/Release), `Scope`.
- `internal/ratelimit` — per-API-key Redis fixed window (INCR+PEXPIRE in one
  Lua script); callers fail open.
Source: `git ls-files internal/**`, `internal/api/router.go`, per-package
`doc.go` and `CLAUDE.md`.

## Main dataflows

- HTTP request (all routes):
  `request -> chi middleware (recover -> request-id -> logging -> metrics -> reject-query-credentials) -> route-scoped auth -> handler -> response`
  (`internal/api/router.go` lines 148-190).

- Chat completion (the precedence-critical path — authoritative detail in
  `docs/design-decisions.md`):
  `POST /v1/chat/completions -> bearer auth -> read raw body once (1 MiB cap) -> hash raw bytes -> parse+validate -> idempotency Check (replay/conflict) -> rate limit (new work only) -> idempotency Begin (pending) -> cache lookup -> Selector.Select -> inference.Client.Do -> mock-llm -> Breaker report -> cache store (2xx+eligible) -> async ledger (inference_requests) -> idempotency resolve (Complete <500 / Release >=500)`
  (`internal/api/chat.go`).

- External backend client:
  `chat handler -> routing.Selector.Select (semaphore + breaker) -> inference.Client.Do (timeout/retry/backoff) -> mock-llm HTTP -> typed error (transient/permanent/canceled) -> failover to next backend or terminal error`
  (`internal/routing/selector.go`, `internal/inference/client.go`).

- DB write (audit ledger, off hot path):
  `handler -> LedgerRecorder.Record -> AsyncLedger buffered queue -> worker goroutine -> InferenceRequestRepo.Insert -> inference_requests table`
  (`internal/api/ledger.go`, `internal/db/inference_repo.go`).

- Auth / tenant:
  `request -> auth.BearerAuth (HMAC-SHA256 key -> KeyStore.GetByHash) -> Principal{TenantID,APIKeyID} in context -> handlers scope cache/idempotency/ledger by that identity`
  (`internal/auth/auth.go`, `internal/api/chat.go`).

- Cache / idempotency / rate limit (separate mechanisms, separate keys):
  `request -> idempotency key = sha256(raw bytes) under scope(tenant+key+method+route); cache key = sha256(tenant:key ‖ canonical parsed body) -> lookups guard execution -> stored result / replay`
  (`internal/idempotency`, `internal/cache`).

- Migrations (startup or `-migrate`):
  `db.RunMigrations -> goose over embedded migrations/*.sql (00001..00006, ordered) -> schema`
  (`internal/db/migrate.go`, `migrations/`).

## Important invariants

Grounded in `DECISIONS.md`, `docs/design-decisions.md`, and code. Preserve these:

- **Docker-free tests, always:** `go test ./...` must pass with no Docker,
  Postgres, or Redis. Real-infra tests are `//go:build integration` only
  (`internal/db/integration_test.go`).
- **Exactly three binaries, forever:** `cmd/gateway-api`, `cmd/control-worker`,
  `cmd/mock-llm` (`DECISIONS.md`).
- **Request precedence is load-bearing** on `/v1/chat/completions`: raw-body-
  once → raw-bytes hash → validate → idempotency Check → rate limit → Begin →
  cache → route/inference → cache store → ledger → resolve. Invalid requests
  never create idempotency records; completed replays are not rate-limited
  (`docs/design-decisions.md`, `internal/api/chat.go`).
- **Idempotency:** Postgres authoritative; `Classify` is the single semantics
  source; a reclaim mints a fresh record id (`id = gen_random_uuid()`) so a
  lapsed/dead owner cannot overwrite the reclaimer; `< 500` → Complete (replay),
  `>= 500` → Release (retryable, not cached); `Idempotency-Key` capped at 255
  chars; `X-Request-ID` never stored/replayed as a header
  (`internal/db/idempotency_repo.go`, `internal/idempotency/idempotency.go`).
- **Cache eligibility:** `stream:false` AND effective temperature ≤ 0.2
  (omitted temperature → OpenAI default 1.0 → not cached; explicit `0` cached);
  store only on backend 2xx; cache and idempotency keys are independent
  (`internal/cache/cache.go`).
- **Rate limit:** per-API-key fixed window; INCR + PEXPIRE in one atomic Lua
  invocation (an expiry can never be orphaned); callers fail open on Redis
  errors (`internal/ratelimit/ratelimit.go`).
- **Routing/concurrency:** `max_in_flight` is a per-process, per-backend
  semaphore, documented as **not** distributed; acquire the semaphore before
  consulting the breaker; `release()` is idempotent (`internal/routing/`).
- **Circuit breaker transition table** closed/open/half-open with a single
  half-open probe; caller-cancellation is verdict-free (`ReportCanceled`)
  (`internal/routing/circuit.go`, `internal/routing/CLAUDE.md`).
- **Retry:** only transient failures (timeout, conn error, 502/503/504) up to
  `RETRY_MAX_ATTEMPTS`; never 400/401/403/404; full-jitter backoff; response
  body capped (`internal/inference/client.go`).
- **Auth:** API keys stored only as `HMAC-SHA256(APP_KEY_HASH_SECRET, key)`;
  admin token compared with constant time; credentials in query params → 400
  before auth (`internal/auth/`, `internal/api/middleware.go`).
- **Migrations are schema-only, ordered, idempotent to re-run;** never edit a
  shipped migration — add a new one (`internal/db/CLAUDE.md`).
- **Observability:** every response carries `X-Request-ID`; logs are redacted
  (never bodies/secrets); metric names are the fixed `aegisroute_*` set
  (`internal/metrics/metrics.go`, `DECISIONS.md`).

## Test and validation commands

Confirmed (from `Makefile`):

- `make verify` — gate: `gofmt -l .` must be empty, then `go vet ./...`, then
  `go test ./...` (no Docker). This is the primary gate.
- `make fmt` / `make vet` / `make test` — the individual steps.
- `make test-integration` — runs `//go:build integration` tests against a real
  Postgres/Redis (requires `DATABASE_URL`, `REDIS_ADDR`).
- `make migrate-up` — `go run ./cmd/gateway-api -migrate` (needs DB).
- `make seed-dev` — `go run ./cmd/gateway-api -seed` (needs DB).
- `make help` — lists all targets.
- Targeted: `go test ./internal/<pkg>/`, add `-race` / `-count=1` as needed.

Stubs — not implemented until Stage 7 (they `echo … ; exit 1`):
`make dev-up`, `make dev-down`, `make logs`, `make verify-e2e`.

CI: `unknown / needs verification` — `.github/workflows/` contains only a
`.gitkeep` (no workflow yet; CI is a Stage-7 deliverable).

## Files usually safe to ignore

- `go.sum` — dependency lock; do not hand-edit.
- `.gitkeep` placeholder files in empty, not-yet-built dirs: `deploy/docker/`,
  `observability/`, `scripts/`, `.github/workflows/`, `cmd/control-worker/`.
- `migrations/embed.go` — tiny `//go:embed` shim (not generated, but rarely the
  thing you need to change; edit the `.sql` files instead).
- Build/test artifacts (all gitignored): `bin/`, `dist/`, `coverage.*`,
  `*.out`, `*.test`, `*.prof`, `tmp/`.
- `.env` (gitignored secrets); read `.env.example` instead.
- No vendor dir, `node_modules`, or lockfiles beyond `go.sum`.

## High-risk areas

- `internal/api/chat.go` — the completion handler; its step ordering is a
  documented invariant (`docs/design-decisions.md`). Reordering breaks
  idempotency/rate-limit/cache correctness.
- `internal/db/idempotency_repo.go` — the atomic `Begin` reclaim SQL and the
  fresh-id invariant; a subtle change can allow stale-response overwrite.
- `internal/routing/circuit.go` + `selector.go` — breaker state machine and the
  semaphore-before-breaker ordering; concurrency-sensitive.
- `internal/inference/client.go` — retry/timeout/cancel classification feeds the
  circuit breaker; mislabeling transient vs permanent poisons routing.
- `migrations/*.sql` — ordered, shipped, schema-only; never edit in place.
- `internal/config/config.go` — validator relationships (e.g. retry budget vs
  `ServerWriteTimeout`) gate startup.

## Evidence index

- `go.mod`: Go version, module path, dependency set (chi, pgx, goose, go-redis,
  prometheus, miniredis, testify, uuid).
- `Makefile`: confirmed validation commands and the Stage-7 stub targets.
- `internal/api/router.go`: route table, middleware order, consumer interfaces.
- `internal/api/chat.go`: completion precedence, idempotency resolve split,
  key cap.
- `internal/db/idempotency_repo.go`: reclaim `gen_random_uuid()` + Release.
- `internal/idempotency/idempotency.go`: Classify/Coordinator/Store contract.
- `internal/cache/cache.go`, `internal/ratelimit/ratelimit.go`: eligibility,
  keying, Lua window.
- `internal/config/config.go`: env var surface, validators.
- `internal/metrics/metrics.go`: the `aegisroute_*` metric names.
- `migrations/` (00001..00006 + `embed.go`): schema, ordering, embed.
- `cmd/gateway-api/main.go`, `cmd/mock-llm/main.go`, `cmd/control-worker/.gitkeep`:
  entrypoints and the not-yet-built worker.
- `git ls-files`, `.gitignore`: tracked vs ignored (note: `CLAUDE.md` files are
  gitignored via `CLAUDE*`; `CODEGRAPH.md` is not).
- Existing docs cross-checked: `PROJECT_STATE.md`, `DECISIONS.md`,
  `docs/REPO_MAP.md`, `docs/STAGE_STATUS.md`, `docs/design-decisions.md`.

## Optional Polycodegraph MCP

Polycodegraph was not configured automatically because the exact supported
language/config format could not be verified from this repository alone. No MCP
or Polycodegraph configuration exists in the repo today (`git ls-files` shows
none), and the repo defines no local tool-config convention to copy.

Future setup should verify:

- whether this repo's primary language (Go) is supported
- the correct graph build command
- the correct MCP server command
- the correct project-local MCP config format

Until then, use this `CODEGRAPH.md` as the manual navigation map. Do not add
guessed MCP JSON.
