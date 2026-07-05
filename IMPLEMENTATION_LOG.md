# AegisRoute — IMPLEMENTATION_LOG

One line per change, appended at commit time. Format:

```
<commit subject or short hash> — <files/areas touched> — <why>
```

---

- `chore: initialize resumable project tracking` — PROJECT_STATE.md, TODO.md, IMPLEMENTATION_LOG.md, DECISIONS.md, README.md (minimal), .gitignore, .env.example, Makefile, go.mod, internal/*/doc.go, .gitkeep'd cmd//migrations//deploy//scripts//docs//observability//.github — establish the memory system, locked decisions, and repo skeleton before any code.
- `feat: foundation config, httperror, logging, metrics scaffold` — internal/config, internal/httperror, internal/observability, internal/metrics (+ tests), go.mod/go.sum deps — Stage 1 foundation packages; `make verify` green with no Docker.
- `heavier test on error boundaries within db migrations, empty json policy fallback issue` (be9c439) — migrations/*.sql (embedded goose, schema only), internal/models (+enums), internal/db (pool, repos, migrate, errors), internal/redisstore, gateway-api -migrate wiring, integration_test — Stage 2 data layer; unit tests Docker-free, integration behind //go:build integration.
- `feat: gateway server, middleware, auth, health, seed, models + admin control-plane` — internal/auth (HashAPIKey/BearerAuth/AdminAuth), internal/api (chi router, middleware chain, health/Pinger, /v1/models, admin CRUD), internal/seed (idempotent Run), cmd/gateway-api full serve/-seed + graceful shutdown, db repo additions (GetByHash→*APIKey, Backend.List/Upsert, Policy.GetByID/Upsert, IsUniqueViolation), Makefile seed-dev, chi/v5 dep — Stage 3 gateway core; all tests Docker-free with fakes.
- `feat: mock-llm, inference client, routing, circuit breaker, chat completions` (uncommitted; branch stage4_sync_inference_v2) — cmd/mock-llm (deterministic fake backend + httptest tests), internal/inference (Client.Do with timeout/retry/full-jitter backoff, typed transient-vs-permanent Error, per-attempt metrics), internal/routing (Breaker state machine + Selector with policy fallback, circuit skip, per-process max_in_flight semaphores, weighted priority tie-break), internal/db/inference_repo.go (+ integration subtest), internal/api chat.go (strict validation, canonical ChatRequest, X-AegisRoute-* headers, circuit reporting, best-effort ledger) + router/Deps wiring, cmd/gateway-api wiring, .env.example MOCK_* — Stage 4 synchronous inference path; all tests Docker-free (fake RoundTripper, fake selector/inference, httptest).
- `review hardening (uncommitted; fold into stage-4 commit)` — internal/inference/client.go (canceled outcome class, non-transient ctx-death, zero-base backoff), internal/routing/circuit.go (ReportCanceled probe return), internal/api/chat.go (case-sensitive raw-key strict validation top-level + per-message, cancel-aware circuit reporting, WithoutCancel+5s ledger ctx, forwardBody built before Select), internal/api/router.go (CircuitReporter.ReportCanceled), tests for all of the above, removed stray zz_scratch_review_test.go — fixes for five adversarial-review findings (circuit/metrics poisoning by client disconnects, lost audit rows on disconnect, case-insensitive strictness bypass, backoff doc mismatch).
