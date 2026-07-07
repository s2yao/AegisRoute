#!/usr/bin/env bash
#
# End-to-end verification for the AegisRoute MVP. Brings up the full Compose
# stack from a clean slate, exercises the sync cache path (MISS -> HIT), the
# async batch path (create -> terminal), and the metrics endpoint, then runs
# the integration test suite against the live stores. Every wait is
# time-bounded so a hung dependency fails the run loudly instead of hanging CI.
#
# Usage: bash scripts/e2e.sh   (or: make verify-e2e)
set -euo pipefail

# --- config ---------------------------------------------------------------
GATEWAY="http://localhost:8080"
WORKER="http://localhost:9100"
API_KEY="sg_dev_key_123"          # LOCAL-ONLY demo key (see .env.example)
READY_ATTEMPTS=30                 # x 2s = 60s cap for stack readiness
READY_SLEEP=2
BATCH_ATTEMPTS=30                 # x 2s = 60s cap for a batch to reach terminal
BATCH_SLEEP=2
HDR_MISS="$(mktemp)"
HDR_HIT="$(mktemp)"

CHAT_BODY='{ "model": "llama-fast",
  "messages": [ {"role": "user", "content": "Return one short sentence about routing."} ],
  "temperature": 0, "max_tokens": 32 }'

BATCH_BODY='{ "requests": [
  { "custom_id": "batch-1", "body": { "model": "llama-fast",
    "messages": [ {"role": "user", "content": "Say batch one."} ], "temperature": 0, "max_tokens": 32 } },
  { "custom_id": "batch-2", "body": { "model": "llama-fast",
    "messages": [ {"role": "user", "content": "Say batch two."} ], "temperature": 0, "max_tokens": 32 } }
] }'

# --- helpers --------------------------------------------------------------
say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  ok: %s\n' "$*"; }
fail() { printf '\nE2E FAILED: %s\n' "$*" >&2; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: '$1' (install it and re-run)"
}

# wait_for_200 polls a URL until it answers 2xx or the attempt cap is hit.
wait_for_200() {
  local url="$1" attempts="$2" sleep_s="$3" i
  for ((i = 1; i <= attempts; i++)); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      ok "$url is up (attempt $i)"
      return 0
    fi
    sleep "$sleep_s"
  done
  fail "timed out after $((attempts * sleep_s))s waiting for $url"
}

cleanup() {
  say "cleanup: tearing down the stack"
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$HDR_MISS" "$HDR_HIT" 2>/dev/null || true
}
trap cleanup EXIT

# --- (a) preflight --------------------------------------------------------
say "preflight: required tooling"
require_cmd docker
require_cmd curl
require_cmd jq
require_cmd go
docker compose version >/dev/null 2>&1 || fail "'docker compose' (v2) is required"
ok "docker, curl, jq, go, docker compose present"

# --- (c) clean slate ------------------------------------------------------
say "clean slate: docker compose down -v --remove-orphans"
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

# --- (d) static + unit gate ----------------------------------------------
say "static analysis + unit tests (Docker-free)"
unformatted="$(gofmt -l .)"
[ -z "$unformatted" ] || fail "gofmt required for:\n$unformatted"
ok "gofmt clean"
go vet ./...
ok "go vet clean"
go test ./...
ok "go test passed"

# --- (e) build + start ----------------------------------------------------
say "docker compose up -d --build"
docker compose up -d --build

# --- (f) bounded readiness wait ------------------------------------------
say "waiting for gateway /readyz and worker /healthz (cap ~60s each)"
wait_for_200 "$GATEWAY/readyz" "$READY_ATTEMPTS" "$READY_SLEEP"
wait_for_200 "$WORKER/healthz" "$READY_ATTEMPTS" "$READY_SLEEP"

# --- (g) sync chat: cache MISS -------------------------------------------
say "sync chat #1 -> expect X-AegisRoute-Cache: MISS"
curl -sS -D "$HDR_MISS" -H "Authorization: Bearer $API_KEY" \
  -H "Idempotency-Key: sync-demo-miss" -H "Content-Type: application/json" \
  -d "$CHAT_BODY" "$GATEWAY/v1/chat/completions" | jq -e '.choices[0].message.content' >/dev/null \
  || fail "chat #1 did not return a completion"
grep -iq "X-AegisRoute-Cache: MISS" "$HDR_MISS" || fail "chat #1 was not a cache MISS (headers:\n$(cat "$HDR_MISS"))"
ok "first call is a MISS and returned a completion"

# --- (h) sync chat: cache HIT (same body, different idempotency key) ------
say "sync chat #2 (same body, new idempotency key) -> expect X-AegisRoute-Cache: HIT"
curl -sS -D "$HDR_HIT" -H "Authorization: Bearer $API_KEY" \
  -H "Idempotency-Key: sync-demo-hit" -H "Content-Type: application/json" \
  -d "$CHAT_BODY" "$GATEWAY/v1/chat/completions" | jq -e '.choices[0].message.content' >/dev/null \
  || fail "chat #2 did not return a completion"
grep -iq "X-AegisRoute-Cache: HIT" "$HDR_HIT" || fail "chat #2 was not a cache HIT (headers:\n$(cat "$HDR_HIT"))"
ok "second call is a HIT (cache served it, no backend call)"

# --- (i) batch create -----------------------------------------------------
say "batch create -> capture job id"
JOB_ID="$(curl -sS -H "Authorization: Bearer $API_KEY" \
  -H "Idempotency-Key: batch-demo-create" -H "Content-Type: application/json" \
  -d "$BATCH_BODY" "$GATEWAY/api/v1/batch-jobs" | jq -r '.id')"
[ -n "$JOB_ID" ] && [ "$JOB_ID" != "null" ] || fail "batch create did not return an id"
ok "created batch job $JOB_ID"

# --- (j) bounded poll to a terminal status --------------------------------
say "polling batch job until terminal (cap ~60s)"
status=""
for ((i = 1; i <= BATCH_ATTEMPTS; i++)); do
  status="$(curl -fsS -H "Authorization: Bearer $API_KEY" \
    "$GATEWAY/api/v1/batch-jobs/$JOB_ID" | jq -r '.status')"
  case "$status" in
    succeeded | partially_failed | failed)
      ok "batch job reached terminal status '$status' (attempt $i)"
      break
      ;;
  esac
  sleep "$BATCH_SLEEP"
done
case "$status" in
  succeeded | partially_failed | failed) ;;
  *) fail "batch job $JOB_ID did not reach a terminal status in $((BATCH_ATTEMPTS * BATCH_SLEEP))s (last: '$status')" ;;
esac

# Items should be present and terminal too.
items_terminal="$(curl -fsS -H "Authorization: Bearer $API_KEY" \
  "$GATEWAY/api/v1/batch-jobs/$JOB_ID/items" \
  | jq '[.[] | select(.status == "succeeded" or .status == "failed")] | length')"
[ "$items_terminal" = "2" ] || fail "expected 2 terminal items, got $items_terminal"
ok "both batch items are terminal"

# --- (k) metrics ----------------------------------------------------------
say "metrics: gateway exposes aegisroute_* series"
curl -sf "$GATEWAY/metrics" | grep -q "aegisroute_" || fail "no aegisroute_ metrics on the gateway"
curl -sf "$WORKER/metrics"  | grep -q "aegisroute_" || fail "no aegisroute_ metrics on the worker"
ok "both processes export aegisroute_* metrics"

# --- (l) integration tests against the live stores ------------------------
say "integration tests against the live stack (go test -tags=integration ./...)"
export DATABASE_URL="postgres://aegisroute:aegisroute@localhost:5432/aegisroute?sslmode=disable"
export REDIS_ADDR="localhost:6379"
export REDIS_DB="0"
export APP_KEY_HASH_SECRET="dev_only_change_me_32_bytes_minimum"
export DEV_API_KEY="sg_dev_key_123"
export ADMIN_TOKEN="dev_admin_token"
export STREAM_KEY="aegisroute:batch_jobs"
export STREAM_GROUP="aegisroute-workers"
export SEED_BACKEND_FAST_URL="http://localhost:8081"
export SEED_BACKEND_CHEAP_URL="http://localhost:8082"
go test -tags=integration -count=1 ./...
ok "integration tests passed"

say "E2E PASSED"
