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

## Stage 4 — Sync inference (NEXT)

- [ ] cmd/mock-llm deterministic OpenAI-compatible backend
- [ ] internal/inference upstream client: timeout, retry (RETRY_* env), per-backend max_in_flight semaphore
- [ ] Circuit breaker (CB_* env) as pure state machine
- [ ] internal/routing backend selection by priority/health
- [ ] /v1/chat/completions (non-streaming; streaming requests get unsupported_streaming)

## Stage 5 — Cache + idempotency + rate limiting

- [ ] internal/cache: response cache, canonicalized cache keys, CACHE_TTL_SECONDS
- [ ] internal/idempotency: Idempotency-Key fast path, IDEMPOTENCY_TTL_SECONDS
- [ ] internal/ratelimit: per-key limiter, RATE_LIMIT_QPS
- [ ] miniredis-backed unit tests for all three

## Stage 6 — Batch jobs + control-worker

- [ ] Queue interface: Redis Streams impl + in-memory fake
- [ ] /api/v1/batch-jobs* endpoints; job/item status machine
- [ ] cmd/control-worker: consumer group, bounded pool (WORKER_CONCURRENCY), WORKER_MAX_ITEM_ATTEMPTS then DLQ, /healthz + /metrics on :9100

## Stage 7 — Docker/Compose/Prometheus/E2E/docs/CI

- [ ] Dockerfiles + docker-compose.yml (postgres, redis, both mock-llms, gateway, worker, prometheus)
- [ ] Prometheus scrape config; make dev-up/dev-down/logs/verify-e2e real
- [ ] E2E verification script; GitHub Actions CI
- [ ] Full README, docs/future-work.md; final verification
