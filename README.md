# AegisRoute

## Assumptions & Tradeoffs

- This README stays minimal until Stage 7 by design (locked scope rule); the
  project's living documentation is `PROJECT_STATE.md`, `TODO.md`,
  `DECISIONS.md`, and `IMPLEMENTATION_LOG.md`.
- `go.mod` declares `go 1.25`; the local toolchain may be newer (installed via
  Homebrew) — the directive, not the toolchain version, is the compatibility
  contract.
- Module path is `github.com/example/aegisroute` until published; rename later
  with `go mod edit -module github.com/<you>/aegisroute && go mod tidy`.
- Config is environment-variables-only (stdlib `os`, no dotenv library).
  Empty-string values are treated as unset, so defaults apply. Host-run Make
  targets that need infra will source `.env` themselves once they exist.
- Validation is split per run mode (`ValidateForMigrate` / `ValidateForSeed` /
  `ValidateForServe`) so one-off operations don't fail on unrelated runtime
  variables. `ValidateForServe` intentionally requires `ADMIN_TOKEN`, since the
  server exposes admin routes from Stage 3.
- The credentials in `.env.example` (`sg_dev_key_123`, `dev_admin_token`) are
  LOCAL-ONLY demo values, safe to commit and never valid outside a local dev
  stack.
- `go test ./...` requires no Docker, Postgres, or Redis — ever. Real-infra
  tests are gated behind `//go:build integration`.
- `internal/metrics.New()` registers only the `aegisroute_*` collectors into a
  fresh registry (no Go runtime/process collectors), keeping `/metrics` output
  small and deterministic for tests; runtime collectors can be added later if
  wanted.
- `observability.Redact` matches secret markers as case-insensitive
  *substrings* (e.g. `X-Api-Key`, `APP_KEY_HASH_SECRET`), preferring false
  positives over leaking a secret.
- Labeled Prometheus counters don't appear in `/metrics` output until first
  increment; unlabeled ones export at 0 immediately. This is standard client
  behavior, not a bug.
