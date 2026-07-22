#!/usr/bin/env bash
#
# Load benchmark for the AegisRoute MVP. Brings up the bench-tuned Compose
# stack (rate limit effectively off, ~40ms simulated backend latency), runs
# fixed load profiles with `hey`, extracts the same numbers from Prometheus to
# prove they are instrumented, and writes docs/benchmarks.md. Single-machine,
# local Docker Compose — not a distributed/cloud load test (caveats in the doc).
#
# Usage: bash scripts/bench.sh   (or: make bench). Requires: docker, curl, jq, hey.
set -euo pipefail

# hey installs to $GOPATH/bin, which may not be on PATH.
export PATH="$PATH:$(go env GOPATH 2>/dev/null)/bin"

# --- config ---------------------------------------------------------------
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.bench.yml)
GATEWAY="http://localhost:8080"
PROM="http://localhost:9090"
API_KEY="sg_dev_key_123"
DURATION="${BENCH_DURATION:-30s}"
CONCURRENCY="${BENCH_CONCURRENCY:-50}"
READY_ATTEMPTS=30
READY_SLEEP=2
BATCH_ITEMS="${BENCH_BATCH_ITEMS:-200}"
BATCH_ATTEMPTS=60
BATCH_SLEEP=1
OUT="docs/benchmarks.md"
TMP="$(mktemp -d)"

say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  ok: %s\n' "$*"; }
warn() { printf '  warn: %s\n' "$*" >&2; }
fail() { printf '\nBENCH FAILED: %s\n' "$*" >&2; exit 1; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || fail "required command not found: '$1'"; }

cleanup() {
  say "cleanup: tearing down the bench stack"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$TMP" 2>/dev/null || true
}
trap cleanup EXIT

wait_for_200() {
  local url="$1" i
  for ((i = 1; i <= READY_ATTEMPTS; i++)); do
    curl -fsS -o /dev/null "$url" 2>/dev/null && { ok "$url up (attempt $i)"; return 0; }
    sleep "$READY_SLEEP"
  done
  fail "timed out after $((READY_ATTEMPTS * READY_SLEEP))s waiting for $url"
}

# hey summary parsers (values in seconds / rps). NOTE: hey prints latency
# percentiles with a literal double-percent ("50%% in 0.0456 secs"), so the
# grep must match "%%". `|| true` keeps a parse miss from aborting under set -e.
hey_rps()  { grep 'Requests/sec' "$1" | awk '{print $2}' || true; }
hey_pct()  { grep "  $2%% in" "$1" | awk '{print $3}' || true; }  # $2 = 50|95|99

# PromQL scalar query.
q() {
  curl -s --data-urlencode "query=$1" "$PROM/api/v1/query" \
    | jq -r '.data.result[0].value[1] // "n/a"'
}

# --- (a) preflight --------------------------------------------------------
say "preflight"
require_cmd docker; require_cmd curl; require_cmd jq
command -v hey >/dev/null 2>&1 || fail "hey not found — install with: go install github.com/rakyll/hey@latest"
"${COMPOSE[@]}" version >/dev/null 2>&1 || fail "docker compose (v2) required"
ok "docker, curl, jq, hey present"

# --- (c) bring up the bench stack -----------------------------------------
say "bringing up the bench stack (rate limit off, ~40ms backend latency)"
"${COMPOSE[@]}" up -d --build
wait_for_200 "$GATEWAY/readyz"
# let the worker settle + prometheus take a scrape
sleep 5

# --- (d) Profile 1 — uncached full path (temperature 0.9 => always BYPASS) -
say "profile 1: uncached (BYPASS) — full auth→route→inference→persist, ${DURATION} @ c=${CONCURRENCY}"
hey -z "$DURATION" -c "$CONCURRENCY" -m POST \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"llama-fast","messages":[{"role":"user","content":"benchmark uncached"}],"temperature":0.9,"max_tokens":32}' \
  "$GATEWAY/v1/chat/completions" | tee "$TMP/uncached.txt" >/dev/null
UNCACHED_RPS="$(hey_rps "$TMP/uncached.txt")"
UNCACHED_P50="$(hey_pct "$TMP/uncached.txt" 50)"
UNCACHED_P95="$(hey_pct "$TMP/uncached.txt" 95)"
UNCACHED_P99="$(hey_pct "$TMP/uncached.txt" 99)"
[ -n "$UNCACHED_RPS" ] && [ -n "$UNCACHED_P95" ] || { cat "$TMP/uncached.txt"; fail "could not parse hey output (profile 1)"; }
ok "uncached: rps=${UNCACHED_RPS} p50=${UNCACHED_P50}s p95=${UNCACHED_P95}s p99=${UNCACHED_P99}s"

# --- (e) Profile 2 — cached path (temperature 0, no key => MISS then HIT) --
say "profile 2: cached (HIT) — warm once, then ${DURATION} @ c=${CONCURRENCY}"
curl -fsS -o /dev/null -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"llama-fast","messages":[{"role":"user","content":"benchmark cached"}],"temperature":0,"max_tokens":32}' \
  "$GATEWAY/v1/chat/completions" || fail "cache warm request failed"
hey -z "$DURATION" -c "$CONCURRENCY" -m POST \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"llama-fast","messages":[{"role":"user","content":"benchmark cached"}],"temperature":0,"max_tokens":32}' \
  "$GATEWAY/v1/chat/completions" | tee "$TMP/cached.txt" >/dev/null
CACHED_RPS="$(hey_rps "$TMP/cached.txt")"
CACHED_P50="$(hey_pct "$TMP/cached.txt" 50)"
CACHED_P95="$(hey_pct "$TMP/cached.txt" 95)"
CACHED_P99="$(hey_pct "$TMP/cached.txt" 99)"
[ -n "$CACHED_RPS" ] && [ -n "$CACHED_P95" ] || { cat "$TMP/cached.txt"; fail "could not parse hey output (profile 2)"; }
ok "cached: rps=${CACHED_RPS} p50=${CACHED_P50}s p95=${CACHED_P95}s p99=${CACHED_P99}s"

# speedup = uncached_p95 / cached_p95 ; rps_ratio = cached_rps / uncached_rps
SPEEDUP="$(awk -v a="$UNCACHED_P95" -v b="$CACHED_P95" 'BEGIN{ if (b>0) printf "%.1f", a/b; else print "n/a" }')"
RPS_RATIO="$(awk -v a="$CACHED_RPS" -v b="$UNCACHED_RPS" 'BEGIN{ if (b>0) printf "%.1f", a/b; else print "n/a" }')"
ok "cache speedup (p95): ${SPEEDUP}x   throughput ratio: ${RPS_RATIO}x"

# --- (g) Batch throughput -------------------------------------------------
# The API caps a batch at 100 items, so BATCH_ITEMS is submitted as
# ceil(BATCH_ITEMS/100) batches; wall-clock is measured from the first submit
# to all jobs terminal, then items/min is computed over the total.
CHUNK=100
say "batch throughput: ${BATCH_ITEMS} items across batches of ≤${CHUNK}, submit → poll all to terminal"
JOB_IDS=()
remaining="$BATCH_ITEMS"; offset=0
BATCH_START="$(date +%s)"
while [ "$remaining" -gt 0 ]; do
  n=$(( remaining > CHUNK ? CHUNK : remaining ))
  body="$(jq -nc --argjson n "$n" --argjson off "$offset" \
    '{requests: [range($n) | {custom_id: ("item-\($off + .)"), body: {model:"llama-fast", messages:[{role:"user", content:("batch \($off + .)")}], temperature:0, max_tokens:32}}]}')"
  jid="$(curl -fsS -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d "$body" "$GATEWAY/api/v1/batch-jobs" | jq -r '.id')"
  [ -n "$jid" ] && [ "$jid" != "null" ] || fail "batch create returned no id"
  JOB_IDS+=("$jid")
  remaining=$(( remaining - n )); offset=$(( offset + n ))
done
bstatus="succeeded"
for jid in "${JOB_IDS[@]}"; do
  js=""
  for ((i = 1; i <= BATCH_ATTEMPTS; i++)); do
    js="$(curl -fsS -H "Authorization: Bearer $API_KEY" "$GATEWAY/api/v1/batch-jobs/$jid" 2>/dev/null | jq -r '.status' 2>/dev/null || true)"
    case "$js" in succeeded|partially_failed|failed) break ;; esac
    sleep "$BATCH_SLEEP"
  done
  case "$js" in
    succeeded) ;;
    partially_failed|failed) bstatus="$js" ;;
    *) fail "batch $jid did not reach terminal in $((BATCH_ATTEMPTS * BATCH_SLEEP))s (last: '$js')" ;;
  esac
done
BATCH_ELAPSED=$(( $(date +%s) - BATCH_START ))
[ "$BATCH_ELAPSED" -lt 1 ] && BATCH_ELAPSED=1
BATCH_PER_MIN="$(awk -v n="$BATCH_ITEMS" -v s="$BATCH_ELAPSED" 'BEGIN{ printf "%.0f", n/(s/60) }')"
NUM_BATCHES="${#JOB_IDS[@]}"
ok "batch: ${BATCH_ITEMS} items (${NUM_BATCHES} batches) -> '${bstatus}' in ${BATCH_ELAPSED}s (~${BATCH_PER_MIN} items/min)"

# --- (h) Prometheus metric extraction (proves the numbers are instrumented) -
say "metric extraction from Prometheus (PromQL)"
sleep 6  # let prometheus scrape the post-load state
PROM_CHAT_P95="$(q 'histogram_quantile(0.95, sum(rate(aegisroute_http_request_duration_seconds_bucket{route="/v1/chat/completions"}[1m])) by (le))')"
PROM_HIT_P95="$(q 'histogram_quantile(0.95, sum(rate(aegisroute_chat_completion_duration_seconds_bucket{cache="hit"}[1m])) by (le))')"
PROM_BYPASS_P95="$(q 'histogram_quantile(0.95, sum(rate(aegisroute_chat_completion_duration_seconds_bucket{cache="bypass"}[1m])) by (le))')"
HIT_RATIO="$(q 'sum(aegisroute_cache_events_total{result="hit"}) / clamp_min(sum(aegisroute_cache_events_total{result=~"hit|miss"}), 1)')"
PROM_BATCH_IPS="$(q 'sum(rate(aegisroute_batch_items_processed_total[1m]))')"
ok "prom chat p95=${PROM_CHAT_P95}s hit-p95=${PROM_HIT_P95}s bypass-p95=${PROM_BYPASS_P95}s hit-ratio=${HIT_RATIO} batch-items/s=${PROM_BATCH_IPS}"

# --- (f) Rate-limit demo (recreate gateway with a low limit) --------------
say "rate-limit demo: recreate gateway with RATE_LIMIT_QPS=5, fire 50 rapid requests"
BENCH_RATE_LIMIT_QPS=5 "${COMPOSE[@]}" up -d gateway-api >/dev/null 2>&1
wait_for_200 "$GATEWAY/readyz"
RL_429=0; RL_OK=0
for i in $(seq 1 50); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d '{"model":"llama-fast","messages":[{"role":"user","content":"rl"}],"temperature":0,"max_tokens":8}' \
    "$GATEWAY/v1/chat/completions" 2>/dev/null || echo 000)"
  case "$code" in 429) RL_429=$((RL_429+1)) ;; 200) RL_OK=$((RL_OK+1)) ;; esac
done
sleep 6
RL_METRIC="$(q 'sum(aegisroute_rate_limited_total)')"
[ "$RL_429" -gt 0 ] && ok "rate limit: ${RL_429}/50 got 429 (metric=${RL_METRIC})" || warn "no 429s observed (metric=${RL_METRIC})"
# restore the effectively-off limit for a clean end state
"${COMPOSE[@]}" up -d gateway-api >/dev/null 2>&1

# --- (i) Circuit-breaker demo (fail the "fast" backend, expect failover) ---
say "circuit-breaker demo: mock-llm-fast MOCK_FAILURE_RATE=1, drive 200 requests"
BENCH_FAST_FAILURE_RATE=1 "${COMPOSE[@]}" up -d mock-llm-fast >/dev/null 2>&1
sleep 3
hey -n 200 -c 20 -m POST \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"llama-fast","messages":[{"role":"user","content":"cb demo"}],"temperature":0.9,"max_tokens":16}' \
  "$GATEWAY/v1/chat/completions" | tee "$TMP/cb.txt" >/dev/null
CB_2XX="$(grep -A6 'Status code distribution' "$TMP/cb.txt" | grep '\[200\]' | awk '{print $2}')"
sleep 6
CB_STATE="$(q 'aegisroute_circuit_breaker_state{backend="mock-llm-fast"}')"
CB_SHORT="$(q 'sum(aegisroute_circuit_breaker_short_circuits_total{backend="mock-llm-fast"})')"
CB_RETRIES="$(q 'sum(aegisroute_backend_retries_total{backend="mock-llm-fast"})')"
CB_OPENS="$(q 'sum(aegisroute_circuit_breaker_transitions_total{backend="mock-llm-fast",to="open"})')"
ok "circuit breaker: state=${CB_STATE} short_circuits=${CB_SHORT} retries=${CB_RETRIES} opens=${CB_OPENS} 200s=${CB_2XX:-?}/200"
[ "${CB_SHORT:-0}" != "n/a" ] && awk -v v="${CB_SHORT:-0}" 'BEGIN{exit !(v>0)}' \
  && ok "breaker short-circuited the failing backend; requests still served by mock-llm-cheap" \
  || warn "expected short-circuits > 0 (timing/threshold dependent)"

# --- (j) write docs/benchmarks.md -----------------------------------------
say "writing $OUT"
CPU_MODEL="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //' || echo unknown)"
CPU_CORES="$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo unknown)"
RAM_GB="$(awk -v b="$(sysctl -n hw.memsize 2>/dev/null || echo 0)" 'BEGIN{ if (b>0) printf "%.0f", b/1073741824; else print "unknown" }')"
[ "$RAM_GB" = "unknown" ] && RAM_GB="$(awk '/MemTotal/{printf "%.0f", $2/1048576}' /proc/meminfo 2>/dev/null || echo unknown)"
NOW="$(date -u '+%Y-%m-%d %H:%M UTC')"

cat > "$OUT" <<EOF
# Benchmarks

Real, reproducible performance numbers for the AegisRoute MVP. Generated by
\`make bench\` (\`scripts/bench.sh\`).

## Environment (read this first — the numbers are only meaningful with it)

- **Single-machine local Docker Compose benchmark** — NOT a distributed or
  cloud load test. Everything (gateway, worker, Postgres, Redis, two mock
  backends, Prometheus, and the \`hey\` load generator) runs on one host, so
  the load generator competes with the system under test for CPU.
- Host: ${CPU_MODEL} — ${CPU_CORES} logical cores, ~${RAM_GB} GB RAM.
- Load generator: \`hey\`, concurrency \`-c ${CONCURRENCY}\`, duration \`-z ${DURATION}\` per profile.
- \`mock-llm\` stands in for a real model backend, with a simulated
  \`MOCK_LATENCY_MS=40\` (+5ms jitter) — the "backend" is deterministic, not a
  real LLM, so these measure the **control plane**, not model inference.
- \`RATE_LIMIT_QPS\` was raised to 1000000 (effectively off) for the throughput
  profiles so they measure the gateway, not the limiter. The rate-limit demo
  temporarily lowers it to 5.
- The response cache was **on**. Headline latency/RPS are \`hey\`'s own numbers
  (exact, computed over every request); the PromQL values show the same story
  is observable in metrics (small gaps are scrape-window/quantile-bucket
  artefacts — prefer the \`hey\` number).
- Generated: ${NOW}.

## Results

| Profile | p50 | p95 | p99 | RPS |
| --- | --- | --- | --- | --- |
| Uncached (BYPASS, full path) | ${UNCACHED_P50}s | ${UNCACHED_P95}s | ${UNCACHED_P99}s | ${UNCACHED_RPS} |
| Cached (HIT) | ${CACHED_P50}s | ${CACHED_P95}s | ${CACHED_P99}s | ${CACHED_RPS} |

- **Cache speedup (p95): ${SPEEDUP}x**, throughput ratio: ${RPS_RATIO}x.
- Cache hit ratio (Prometheus): **${HIT_RATIO}**.
- Batch throughput: **${BATCH_ITEMS} items (${NUM_BATCHES} batches of ≤100) → '${bstatus}' in ${BATCH_ELAPSED}s (~${BATCH_PER_MIN} items/min)**; Prometheus \`rate(aegisroute_batch_items_processed_total[1m])\` = ${PROM_BATCH_IPS}/s.
- Rate-limit demo (QPS=5): **${RL_429}/50 requests got HTTP 429**; \`aegisroute_rate_limited_total\` = ${RL_METRIC}.
- Circuit-breaker demo (fast backend forced to fail): state=${CB_STATE} (0=closed,1=half-open,2=open), short-circuits=${CB_SHORT}, retries=${CB_RETRIES}, opens=${CB_OPENS}; **${CB_2XX:-?}/200 requests still succeeded** (served by mock-llm-cheap via failover).

### Prometheus cross-check (same numbers, from metrics)

| Metric (PromQL) | Value |
| --- | --- |
| \`histogram_quantile(0.95, …http_request_duration…{route="/v1/chat/completions"}…)\` | ${PROM_CHAT_P95}s |
| \`histogram_quantile(0.95, …chat_completion_duration…{cache="hit"}…)\` | ${PROM_HIT_P95}s |
| \`histogram_quantile(0.95, …chat_completion_duration…{cache="bypass"}…)\` | ${PROM_BYPASS_P95}s |
| cache hit ratio | ${HIT_RATIO} |
| \`sum(rate(aegisroute_batch_items_processed_total[1m]))\` | ${PROM_BATCH_IPS}/s |

## Reproduce

\`\`\`sh
make bench           # or: bash scripts/bench.sh
\`\`\`

The exact \`hey\` invocations and PromQL queries live in \`scripts/bench.sh\`.
Numbers vary run to run and machine to machine; re-run to regenerate this file.
EOF

ok "wrote $OUT"

say "BENCH SUMMARY"
cat <<EOF
  uncached (BYPASS): p50=${UNCACHED_P50}s p95=${UNCACHED_P95}s p99=${UNCACHED_P99}s rps=${UNCACHED_RPS}
  cached   (HIT):    p50=${CACHED_P50}s p95=${CACHED_P95}s p99=${CACHED_P99}s rps=${CACHED_RPS}
  cache speedup (p95): ${SPEEDUP}x    throughput ratio: ${RPS_RATIO}x    hit ratio: ${HIT_RATIO}
  batch: ${BATCH_ITEMS} items in ${BATCH_ELAPSED}s (~${BATCH_PER_MIN} items/min)
  rate limit: ${RL_429}/50 -> 429 (metric ${RL_METRIC})
  circuit breaker: state=${CB_STATE} short_circuits=${CB_SHORT} retries=${CB_RETRIES} opens=${CB_OPENS} 200s=${CB_2XX:-?}/200
  wrote: $OUT
EOF
say "BENCH PASSED"
