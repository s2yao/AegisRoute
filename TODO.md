# AegisRoute — TODO

One checklist per stage. The active stage is the first unchecked one; work on
that stage only. Sub-tasks for future stages are expanded when the stage
starts.

## Stage 1 — Foundations (DONE)

- [x] Memory files: PROJECT_STATE.md, TODO.md, IMPLEMENTATION_LOG.md, DECISIONS.md; minimal README (title + Assumptions & Tradeoffs only)
- [x] Repo skeleton: go.mod (go 1.25, github.com/example/aegisroute), internal/* doc.go packages, .gitkeep'd cmd/, migrations/, deploy/docker/, scripts/, docs/, observability/, .github/workflows/
- [x] .gitignore (Go + Docker; .env ignored, .env.example tracked) — verify essential files still tracked
- [x] .env.example with all variables, commented, demo creds marked LOCAL-ONLY
- [x] Makefile final target list; future-stage targets fail with "not implemented until Stage X"
- [x] internal/config: Config, Load(), ValidateForMigrate/ValidateForSeed/ValidateForServe
- [x] internal/httperror: APIError, {"error":{...}} envelope, Write(), named codes
- [x] internal/observability: NewLogger (slog JSON), request-id context helpers, Redact
- [x] internal/metrics: Metrics struct, New() fresh registry, Handler(), all aegisroute_* collectors
- [x] Tests: config defaults/overrides/validators, httperror exact JSON + request_id, observability redact + round-trip, metrics no-panic + handler output
- [x] Definition of Done: gofmt clean, go vet, go build, go test, make verify, make help
- [x] Update memory files; commit in two commits (tracking, then foundation)

## Stage 2 — Data layer (DONE)

- [x] goose migrations (embedded via //go:embed), schema only
- [x] internal/db: pgx repositories (raw SQL); consumer-declared repo interfaces
- [x] internal/redisstore: Redis client construction/helpers
- [x] internal/models: shared domain types
- [x] -migrate mode wired in gateway-api; make migrate-up / test-integration real
- [x] //go:build integration tests against real PG/Redis

## Stage 3 — Gateway core (DONE)

- [x] cmd/gateway-api HTTP server (chi), graceful shutdown (signal.NotifyContext + srv.Shutdown)
- [x] Middleware chain: recover→request-id→logging (redacted)→metrics→reject-query-credentials, then route-scoped auth
- [x] Auth: bearer API keys (HMAC-SHA256 lookup, internal/auth), admin token (X-Admin-Token, ConstantTimeCompare); fixed routing table; 400 on credentials in query params
- [x] /healthz, /readyz (Pinger interface), /metrics; /v1/models (bearer, dedup by model_name)
- [x] Admin control-plane CRUD: /api/v1/backends{,/{id}}, /api/v1/routing-policies{,/{id}} (soft-disable, immutable-field protection)
- [x] internal/seed idempotent seeder; -seed mode; AEGISROUTE_AUTO_MIGRATE/AUTO_SEED startup paths; make seed-dev real
- [x] db additions: APIKeyRepo.GetByHash→*APIKey, BackendRepo.List+Upsert, RoutingPolicyRepo.GetByID+Upsert, IsUniqueViolation
- [x] Tests (Docker-free, fakes): auth, middleware, health, /v1/models, admin CRUD, seed, error-shape

## Stage 4 — Sync inference (DONE — uncommitted; see PROJECT_STATE.md)

- [x] cmd/mock-llm deterministic OpenAI-compatible backend (hash-derived content, fixed `created`, MOCK_* env incl. latency/jitter/failure-rate, /healthz; httptest-covered)
- [x] internal/inference Client.Do: per-attempt BACKEND_TIMEOUT_MS, retry RETRY_MAX_ATTEMPTS with exp backoff + full jitter (RETRY_BASE_MS/RETRY_MAX_MS), transient-only retry (timeout/conn/502/503/504), typed Error{Transient}, bodies always closed, per-attempt backend metrics
- [x] Circuit breaker (internal/routing/circuit.go): closed/open/half-open per backend, CB_FAILURE_THRESHOLD/CB_COOLDOWN_MS, single half-open probe, state listener → aegisroute_circuit_breaker_state gauge (0/1/2); full transition-table tests
- [x] internal/routing Selector.Select → (Selection{Backend,PolicyName,Strategy}, release, err): enabled policy or in-memory `default` fallback, defensive row filter, skips open circuits, per-process max_in_flight semaphores (fail-over when full), priority order, weighted tie-break with injectable rand.Source
- [x] internal/db InferenceRequestRepo.Insert (+ integration-test coverage)
- [x] POST /v1/chat/completions: MaxBytesReader 1 MiB (→413), strict DisallowUnknownFields validation, *float64/*int for temperature/max_tokens, stop string-or-array normalization, stream:true→400 unsupported_streaming, X-AegisRoute-Backend/-Routing-Policy headers, circuit outcome reporting, best-effort ledger row
- [x] gateway-api wiring (shared Breaker for Selector + handler), .env.example MOCK_* block
- [x] Definition of Done: gofmt/vet/build/test all clean, Docker-free

## Stage 5 — Cache + idempotency + rate limiting (DONE — uncommitted; see PROJECT_STATE.md)

- [x] internal/cache: Eligible (stream:false + effective temp ≤ 0.2; omitted → 1.0, explicit 0 cacheable), CanonicalBody (sorted keys, array order preserved), Key = sha256(scope ‖ canonical body), Redis Get/Put with CACHE_TTL_SECONDS; miniredis tests (stability, TTL expiry, corrupt-entry fail-open)
- [x] internal/idempotency: Classify (single semantics source), IdempotencyStore (Lookup/Begin/Complete) satisfied by db.IdempotencyRepo, Coordinator (Check/Begin/Complete), Scope (tenant+key+method+route, Stage-6 reusable), IDEMPOTENCY_TTL_SECONDS + lock TTL; in-memory-fake tests (replay/conflict/in-progress/stale-reclaim/expired/race)
- [x] internal/db/idempotency_repo.go: atomic INSERT…ON CONFLICT…WHERE reclaim on DB clock; integration subtest (insert/conflict/reclaim/complete/expiry/scope isolation)
- [x] internal/ratelimit: per-API-key fixed window, INCR+PEXPIRE in one Lua invocation (expiry can never be lost), RATE_LIMIT_QPS; miniredis + FastForward tests; fail-open on Redis errors
- [x] Chat handler exact precedence: raw body once → raw-bytes hash → validate → idempotency Check → rate limit (new work only) → Begin → cache lookup → route/inference → cache store (2xx+eligible) → ledger (cache_result set; HIT rows backend_id NULL) → Complete on every path; X-AegisRoute-Cache HIT|MISS|BYPASS; replays never reuse stored X-Request-ID
- [x] rateLimitMiddleware on GET /v1/models (shared per-key budget; chat checks inline); 429 rate_limited + aegisroute_rate_limited_total; aegisroute_cache_events_total{result}
- [x] cmd/gateway-api wiring (lock TTL = 2× ServerWriteTimeout); docs/design-decisions.md precedence note
- [x] Handler integration tests (miniredis + fakes): MISS→HIT (different idempotency keys)→429; replay skips rate limit; changed-body 409; concurrent same-key → one pending record + 409 in-progress; invalid/429 create no records; error responses complete + replay; /v1/models 429 + window refill
- [x] Definition of Done: gofmt/vet/build/test all clean, Docker-free (also -race)

## Stage 6 — Batch jobs + control-worker (DONE — uncommitted; see PROJECT_STATE.md)

- [x] `internal/redisstore` Queue interface (Publish/Consume/Ack/Claim/PublishDLQ) + Message; Redis Streams adapter (XADD, XGROUP MKSTREAM at offset 0, XREADGROUP per-instance consumer, XACK, XAUTOCLAIM, `:dlq` XADD); Consume never auto-acks; in-memory MemQueue fake with publish-error injection + DLQ/ack/inflight introspection
- [x] `internal/jobs` pure status machines (ValidJobTransition, ValidItemTransition, AggregateJobStatus) + JobStore interface (CreateWithItemsAndOutbox, Get/List/Items tenant-scoped, MarkJobRunning, RequeueRunningItems, ClaimNextQueuedItem, UpdateItemTerminal, RecomputeAndUpdateJobStatus, PendingOutbox, MarkOutboxPublished, MarkOutboxFailedAttempt) + in-memory MemStore
- [x] `internal/db/job_repo.go` JobRepo: transactional create (job+items+outbox in one tx); atomic claim via `FOR UPDATE SKIP LOCKED` with claim-time exhaustion; terminal-immutable item update (`WHERE status='running'`); status recompute in SQL mirroring AggregateJobStatus; outbox drain/publish/fail marks; tenant-scoped reads
- [x] `internal/api` batch handlers: POST /api/v1/batch-jobs (10 MiB cap, MVP schema validation reusing Stage-4 chat validation, same-model rule, 1..100 items, unique custom_id, Stage-5 idempotency precedence, exactly one job-level publish, outbox stays pending on publish failure); GET list/get/items all tenant-filtered; BatchJobStore + JobPublisher Deps interfaces; wired into bearer group
- [x] `internal/worker` bounded pool (semaphore = WORKER_CONCURRENCY), per-item claim loop, shared routing+inference (never HTTP to gateway-api), durable terminal write before Ack, Ack only after RecomputeAndUpdateJobStatus, crash-recovery requeue of stranded running items, WORKER_MAX_ITEM_ATTEMPTS → DLQ, periodic XAUTOCLAIM reclaim loop + outbox-drain loop, panic-safe pool goroutines
- [x] `cmd/control-worker/main.go`: config→logger→pgx pool→redis→metrics→queue/selector/inference/store/worker; consume + reclaim + outbox tickers via worker.Run; /healthz + /metrics on WORKER_METRICS_PORT; graceful shutdown on SIGINT/SIGTERM
- [x] Metrics: BatchJobsCreatedTotal (create), BatchItemsProcessedTotal{outcome} (worker), WorkerFailuresTotal (worker-level failures)
- [x] Tests (Docker-free): pure status machine (all transitions + 3 aggregations); MemStore (tenant scoping, atomic concurrent claims never duplicate, exhaustion, terminal immutability, aggregation, outbox lifecycle); queue adapter over miniredis (publish→consume→ack, Consume-no-auto-ack + Claim recovers stranded via SetTime, DLQ stream, backlog before group); MemQueue fake; worker pool (concurrency peak ≤ limit, redelivery skips terminal items, Ack-after-durable-update via instrumented fakes, exhaustion→DLQ+job failed, partial-failure aggregation, outbox-drain retries then marks published); batch endpoints (persist+one publish, publish-failure keeps outbox pending, GET/items/list, idempotency replay, cross-tenant 404, validation matrix); JobRepo integration subtests (`//go:build integration`)
- [x] Definition of Done: gofmt/vet/build/test all clean, Docker-free (also `-race` on worker/redisstore/jobs/api); `go build ./cmd/control-worker` green

## Stage 7 — Docker/Compose/Prometheus/E2E/docs/CI (DONE — MVP complete; uncommitted)

- [x] Dockerfiles: `deploy/docker/{gateway-api,control-worker,mock-llm}.Dockerfile` — multi-stage, pinned `golang:1.25.11` build, static CGO-free binaries, `gcr.io/distroless/static-debian12:nonroot` final (non-root, no `:latest`); `.dockerignore` keeps the build context lean
- [x] `docker-compose.yml`: postgres (`pg_isready` healthcheck + `pgdata` volume), redis (`redis-cli ping` healthcheck), gateway-api (auto-migrate + auto-seed, `depends_on` healthy PG/Redis, :8080), control-worker (:9100), mock-llm-fast (:8081) + mock-llm-cheap (:8082) both `llama-fast`, prometheus (:9090); service-name networking, no platform pins
- [x] `observability/prometheus.yml`: scrape `gateway-api:8080` + `control-worker:9100`
- [x] `scripts/e2e.sh`: `set -euo pipefail`, preflight (docker/curl/jq/go), cleanup trap (`down -v --remove-orphans`), clean-slate start, gofmt/vet/test gate, `up -d --build`, bounded readiness waits, chat MISS→HIT assertions, batch create → bounded terminal poll → items terminal, gateway+worker `aegisroute_*` metrics, live `go test -tags=integration ./...`; all waits time-bounded
- [x] Makefile: real `dev-up`/`dev-down`/`logs`/`verify-e2e` (→ `bash scripts/e2e.sh`); `migrate-up`/`seed-dev` remained real from Stages 2–3
- [x] `.github/workflows/ci.yml`: `unit` job (gofmt/vet/test, Docker-free) + `integration` job (postgres+redis services → migrate → `go test -tags=integration ./...`)
- [x] `README.md`: overview, not-a-chatbot, not-a-thin-proxy, mermaid diagram, quickstart, LOCAL-ONLY creds, curl + cache-HIT + batch + metrics demos, Developer Operations, Assumptions & Tradeoffs, failure modes, resume link
- [x] `docs/`: architecture.md, api.md, failure-modes.md, resume-bullets.md, future-work.md (non-MVP only; design-decisions.md already present)
- [x] Definition of Done: gofmt/vet/test clean; `make verify-e2e` end-to-end (see PROJECT_STATE.md for live-run status)
