# AegisRoute — Stage Status

At-a-glance status board. Task-level detail lives in `TODO.md`; this file is
the quick summary. Update the status column whenever a stage's state
changes; do not duplicate `TODO.md`'s checklist items here.

| # | Stage | Status | Key deliverables |
| --- | --- | --- | --- |
| 1 | Foundations | ✅ Done, committed | `internal/config`, `internal/httperror`, `internal/observability`, `internal/metrics` |
| 2 | Data layer | ✅ Done, committed | goose migrations, `internal/db` repos, `internal/redisstore` client, `internal/models` |
| 3 | Gateway core | ✅ Done, committed | chi server, middleware chain, auth, health/ready, `/v1/models`, admin CRUD, seeder |
| 4 | Sync inference | ✅ Done, committed | `cmd/mock-llm`, `internal/inference.Client`, `internal/routing.Selector` + `Breaker`, `POST /v1/chat/completions` |
| 5 | Cache + idempotency + rate limiting | ✅ Done, **uncommitted** | `internal/cache`, `internal/idempotency` (+ `db.IdempotencyRepo`), `internal/ratelimit`, `X-AegisRoute-Cache` header, precedence note in `docs/design-decisions.md` |
| 6 | Batch jobs + control-worker | ⬜ Next | `Queue` interface (`internal/jobs`), Redis Streams impl, `/api/v1/batch-jobs*`, `cmd/control-worker` |
| 7 | Docker/Compose/Prometheus/E2E/CI | ⬜ Not started | Dockerfiles, `docker-compose.yml`, Prometheus scrape config, E2E script, CI, full README |

**Definition of Done, every stage:** `gofmt -l .` empty, `go vet ./...`
clean, `go build ./...`, `go test ./...` — all Docker/Postgres/Redis-free
(`make verify`). Real-infra checks are `//go:build integration` only, run
via `make test-integration`.

**Rule:** work the first ⬜ stage only. Do not create a later stage's source
files, Docker assets, CI, or README sections early — see the scope table in
`PROJECT_STATE.md`.

For exactly what shipped in each done stage and any open follow-ups, see the
stage's section in `TODO.md` and its entries in `IMPLEMENTATION_LOG.md`.
