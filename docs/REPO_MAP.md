# AegisRoute — Repository Map

A directory-by-directory guide to what exists and how it connects. For
*why* things are built this way, see `DECISIONS.md`. For *what stage* built
something, see `STAGE_STATUS.md`. This file describes structure only and is
updated whenever a package's responsibility changes — it is not a stage log.

## Binaries (`cmd/`)

Exactly three, forever (see `DECISIONS.md`):

| Binary | Role | Port |
| --- | --- | --- |
| `cmd/gateway-api` | HTTP entry point. Flags: `-migrate` (apply schema, exit), `-seed` (idempotent demo data, exit), no flag = serve. | `:8080` |
| `cmd/control-worker` | Consumes batch jobs from the Redis Stream with a bounded pool. **Stage 6, not yet implemented** (dir exists, empty). | `:9100` |
| `cmd/mock-llm` | Deterministic fake OpenAI-compatible backend; content is a hash of the request body so identical input → identical output. Two instances run in Compose ("fast", "cheap"). | `:8081` / `:8082` |

## Request flow (as of Stage 4)

```
Client
  → gateway-api (chi router, internal/api)
      middleware: recover → request-id → logging → metrics → reject-query-credentials
      → route-scoped auth (internal/auth): bearer for /v1/*, admin token for /api/v1/backends*, /api/v1/routing-policies*
      → handler
          /healthz, /readyz, /metrics            (public)
          /v1/models                             (bearer)
          /v1/chat/completions                   (bearer) → internal/routing.Selector.Select
                                                            → internal/inference.Client.Do → mock-llm
                                                            → internal/routing.Breaker (outcome report)
                                                            → internal/db.InferenceRequestRepo.Insert (ledger)
          /api/v1/backends*, /api/v1/routing-policies*  (admin)
```

## `internal/` packages

Grouped by role; ✅ = implemented, ⏳ = scaffold only (`doc.go`, no logic yet —
do not build ahead of `TODO.md`'s active stage).

**Foundation (Stage 1)**
- `internal/config` ✅ — hand-rolled env-var config (stdlib `os` only, no
  dotenv lib). `Load()` + three validators (`ValidateForMigrate/Seed/Serve`)
  so one-off ops don't fail on unrelated variables. See its own doc comments
  for the full variable list; `.env.example` mirrors it.
- `internal/httperror` ✅ — the one error envelope
  (`{"error":{"code","message","request_id"}}`) and `Write()`. Named codes
  are a fixed, closed set — see `DECISIONS.md`.
- `internal/observability` ✅ — `slog` JSON logger, request-id context
  helpers, `Redact` (the single gate against logging secrets).
- `internal/metrics` ✅ — one non-global `prometheus.Registry` per process;
  every `aegisroute_*` collector is defined in `DECISIONS.md`'s table.

**Data layer (Stage 2)**
- `internal/models` ✅ — shared domain structs (`Tenant`, `APIKey`,
  `ModelBackend`, `RoutingPolicy`, `InferenceRequest`, `BatchJob`,
  `BatchJobItem`, …) and the four typed status enums
  (`JobStatus`/`ItemStatus`/`BackendKind`/`CircuitState`) with
  `Parse*`/`IsValid` mirroring the schema's `CHECK` constraints.
- `internal/db` ✅ — pgx pool + embedded goose migrations + pgx-backed
  repositories, raw SQL, no ORM. See `internal/db/CLAUDE.md`.
- `internal/redisstore` ✅ (client construction only) — the shared
  `go-redis/v9` client and the batch-stream key/group names; stream
  read/write helpers arrive with Stage 6.
- `migrations/` ✅ — goose SQL files, embedded via `//go:embed`; schema only,
  never secrets or seed rows (`DECISIONS.md`).

**Gateway core (Stage 3)**
- `internal/api` ✅ — the chi router (`NewRouter`), middleware chain, health
  probes, `/v1/models`, admin CRUD for backends/routing-policies, and (as of
  Stage 4) the chat-completions handler. Declares its own consumer-side
  interfaces (`BackendStore`, `PolicyStore`, `Pinger`, `ChatSelector`,
  `InferenceDoer`, `CircuitReporter`, `InferenceRequestStore`) so it never
  imports `pgx` or a concrete routing/inference type directly.
- `internal/auth` ✅ — `BearerAuth` (HMAC-SHA256 API-key lookup) and
  `AdminAuth` (constant-time token compare) middleware; `Principal` context
  helpers.
- `internal/seed` ✅ — idempotent demo-data seeder (`Run`), driven by
  `-seed` or `AEGISROUTE_AUTO_SEED`.

**Sync inference (Stage 4)**
- `internal/inference` ✅ — `Client.Do(ctx, backend, body)`: one outbound
  HTTP call with per-attempt timeout, transient-only retry with full-jitter
  backoff, typed transient/permanent/canceled errors. Shared by the gateway
  handler and (later) the batch worker — no HTTP hop between our own
  services.
- `internal/routing` ✅ — `Selector.Select` (backend choice, policy
  application, per-process `max_in_flight` semaphores, priority + weighted
  tie-break) and `Breaker` (the circuit breaker state machine). See
  `internal/routing/CLAUDE.md`.

**Not yet built (Stage 5–7 — scaffolds only, doc.go describes intended role)**
- `internal/cache` ⏳ — response cache, canonicalized cache keys.
- `internal/idempotency` ⏳ — `Idempotency-Key` fast path.
- `internal/ratelimit` ⏳ — per-key QPS limiter.
- `internal/jobs` ⏳ — batch-job domain logic, status machine, the `Queue`
  interface (Redis Streams impl + in-memory fake). See
  `internal/jobs/CLAUDE.md` (also covers Redis Streams — there is no
  separate `internal/queue` package in this repo).
- `internal/worker` ⏳ — consumer-group pool backing `cmd/control-worker`.

## Top-level docs

| File | Purpose |
| --- | --- |
| `CLAUDE.md` | Entry point for AI assistants — points here and to memory files. |
| `PROJECT_STATE.md` | Resumable memory: what's done, current state, how to resume. |
| `TODO.md` | Per-stage checklist; the active stage is the first unchecked one. |
| `DECISIONS.md` | Locked decisions + rationale; never contradicted, only appended. |
| `IMPLEMENTATION_LOG.md` | One line per commit: what changed and why. |
| `docs/REPO_MAP.md` | This file. |
| `docs/STAGE_STATUS.md` | Per-stage done/next/not-started at a glance. |
| `README.md` | Deliberately minimal until Stage 7 (locked scope rule). |
