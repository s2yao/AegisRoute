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
