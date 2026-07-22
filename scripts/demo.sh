#!/usr/bin/env bash
#
# Interactive demo driver for AegisRoute. Reads declarative scenario files from
# demo/scenarios/*.json and fires them at a running demo stack
# (make demo-up, i.e. docker-compose.yml + docker-compose.demo.yml), then
# reports what the control plane did: status-code distribution, cache
# HIT/MISS split, backend-call deltas, breaker state, batch throughput.
#
# Usage:
#   bash scripts/demo.sh              # interactive menu
#   bash scripts/demo.sh list         # list scenarios
#   bash scripts/demo.sh <scenario>   # run one scenario (e.g. cache-storm)
#
# Requires: curl (7.84+ for %header{}), jq. docker is needed only by the
# rate-limit-burst and backend-outage scenarios (they stop/recreate one
# compose service). Same style and helpers as scripts/bench.sh.
set -euo pipefail

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.demo.yml)
GATEWAY="${DEMO_GATEWAY:-http://localhost:8080}"
GRAFANA="${DEMO_GRAFANA:-http://localhost:3000}"
CONSOLE_URL="${DEMO_CONSOLE:-http://localhost:8000}"
API_KEY="${DEMO_API_KEY:-sg_dev_key_123}"
SCEN_DIR="demo/scenarios"
READY_ATTEMPTS=30
READY_SLEEP=2
TMP="$(mktemp -d)"

say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  ok: %s\n' "$*"; }
note() { printf '  %s\n' "$*"; }
warn() { printf '  warn: %s\n' "$*" >&2; }
fail() { printf '\nDEMO FAILED: %s\n' "$*" >&2; exit 1; }

cleanup() { rm -rf "$TMP" 2>/dev/null || true; }
trap cleanup EXIT

require_cmd() { command -v "$1" >/dev/null 2>&1 || fail "required command not found: '$1'"; }

wait_ready() {
  local i
  for ((i = 1; i <= READY_ATTEMPTS; i++)); do
    curl -fsS -o /dev/null "$GATEWAY/readyz" 2>/dev/null && return 0
    sleep "$READY_SLEEP"
  done
  fail "gateway not ready after $((READY_ATTEMPTS * READY_SLEEP))s — start the demo stack with: make demo-up"
}

# Sum every sample of a metric family (fixed-string match, comments excluded)
# straight from a /metrics scrape — instant, no Prometheus scrape lag. The
# inner `|| true` keeps a zero-match grep (a counter not yet exported) from
# tripping pipefail; the awk `s+0` then correctly yields 0.
msum() { # msum <url> <fixed-string>
  curl -sf "$1/metrics" 2>/dev/null | grep -v '^#' | { grep -F "$2" || true; } | awk '{s+=$NF} END{printf "%.0f", s+0}'
}

# One timed request; prints "<http_code> <time_total_seconds>".
timed_request() { # timed_request <body-json> [extra curl args...]
  local body="$1"; shift
  curl -s -o /dev/null -w '%{http_code} %{time_total}' \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    "$@" -d "$body" "$GATEWAY/v1/chat/completions"
}

# Fire <total> POSTs at <concurrency>; one result line per request into $4:
# "<http_code> <x-aegisroute-cache or ->". Uses xargs -P for concurrency; the
# request parameters travel to the xargs children as exported BURST_* vars
# (a prefix assignment on `seq` would not reach them).
fire_burst() { # fire_burst <total> <concurrency> <body-json> <outfile> [extra header]
  local total="$1" conc="$2" out="$4"
  export BURST_BODY="$3" BURST_HDR="${5:-}" BURST_URL="$GATEWAY" BURST_KEY="$API_KEY"
  export BURST_WFMT='%{http_code} %header{x-aegisroute-cache}\n'
  [ "$CURL_HAS_HEADER_FMT" = "1" ] || export BURST_WFMT='%{http_code} -\n'
  seq 1 "$total" | xargs -P "$conc" -I{} sh -c '
    if [ -n "$BURST_HDR" ]; then
      curl -s -o /dev/null -w "$BURST_WFMT" \
        -H "Authorization: Bearer $BURST_KEY" -H "Content-Type: application/json" \
        -H "$BURST_HDR" -d "$BURST_BODY" "$BURST_URL/v1/chat/completions" 2>/dev/null || echo "000 -"
    else
      curl -s -o /dev/null -w "$BURST_WFMT" \
        -H "Authorization: Bearer $BURST_KEY" -H "Content-Type: application/json" \
        -d "$BURST_BODY" "$BURST_URL/v1/chat/completions" 2>/dev/null || echo "000 -"
    fi' > "$out"
}

count_field() { # count_field <file> <field-no> <value>
  awk -v f="$2" -v v="$3" '$f == v {n++} END{print n+0}' "$1"
}

status_summary() { # status_summary <file>
  awk '{print $1}' "$1" | sort | uniq -c | sort -rn | awk '{printf "    %s x HTTP %s\n", $1, $2}'
}

ms() { awk -v s="$1" 'BEGIN{printf "%.1f", s*1000}'; }

scenario_field() { jq -r "$2" "$SCEN_DIR/$1.json"; }

# --- scenario runners -------------------------------------------------------

run_chat_burst() { # cache-storm
  local name="$1"
  local total conc body
  total="$(scenario_field "$name" .total)"
  conc="$(scenario_field "$name" .concurrency)"
  body="$(jq -c .body "$SCEN_DIR/$name.json")"

  local hits0 backend0
  hits0="$(msum "$GATEWAY" 'aegisroute_cache_events_total{result="hit"}')"
  backend0="$(msum "$GATEWAY" 'aegisroute_backend_requests_total')"

  say "timing one cold request (cache MISS -> real backend call)"
  local r code t
  r="$(timed_request "$body")"; code="${r%% *}"; t="${r##* }"
  [ "$code" = "200" ] || fail "warm-up request got HTTP $code"
  ok "MISS took $(ms "$t")ms (traverses auth -> route -> ~40ms mock inference -> persist)"

  say "storm: $total identical requests, $conc concurrent"
  fire_burst "$total" "$conc" "$body" "$TMP/burst.txt"
  status_summary "$TMP/burst.txt"
  local nhit nmiss
  nhit="$(count_field "$TMP/burst.txt" 2 HIT)"
  nmiss="$(count_field "$TMP/burst.txt" 2 MISS)"
  [ "$CURL_HAS_HEADER_FMT" = "1" ] && note "X-AegisRoute-Cache: $nhit HIT / $nmiss MISS"

  say "timing one warm request (cache HIT -> served from Redis)"
  r="$(timed_request "$body")"; code="${r%% *}"; t="${r##* }"
  [ "$code" = "200" ] || fail "warm request got HTTP $code"
  ok "HIT took $(ms "$t")ms"

  local hits1 backend1
  hits1="$(msum "$GATEWAY" 'aegisroute_cache_events_total{result="hit"}')"
  backend1="$(msum "$GATEWAY" 'aegisroute_backend_requests_total')"
  ok "cache hits +$((hits1 - hits0)); backend calls +$((backend1 - backend0)) (only the misses reached a backend)"
}

run_idempotency() { # idempotency-replay
  local name="$1"
  local dups replays body key
  dups="$(scenario_field "$name" .concurrent_duplicates)"
  replays="$(scenario_field "$name" .replays)"
  body="$(jq -c .body "$SCEN_DIR/$name.json")"
  key="demo-replay-$(date +%s)-$RANDOM"

  say "phase 1: $dups concurrent requests race in with the SAME fresh Idempotency-Key"
  fire_burst "$dups" "$dups" "$body" "$TMP/dups.txt" "Idempotency-Key: $key"
  status_summary "$TMP/dups.txt"
  local n200 n409
  n200="$(count_field "$TMP/dups.txt" 1 200)"
  n409="$(count_field "$TMP/dups.txt" 1 409)"
  if [ "$n200" = "1" ] && [ "$n409" = "$((dups - 1))" ]; then
    ok "exactly one executed; $n409 got 409 (in progress) — no double work under a race"
  else
    note "expected 1x200 + $((dups - 1))x409; got ${n200}x200 + ${n409}x409 (timing-dependent: stragglers arriving after completion replay as 200)"
  fi

  say "phase 2: $replays replays of the completed key"
  local backend0 backend1
  backend0="$(msum "$GATEWAY" 'aegisroute_backend_requests_total')"
  fire_burst "$replays" "$replays" "$body" "$TMP/replays.txt" "Idempotency-Key: $key"
  status_summary "$TMP/replays.txt"
  backend1="$(msum "$GATEWAY" 'aegisroute_backend_requests_total')"
  local delta=$((backend1 - backend0))
  if [ "$delta" -eq 0 ]; then
    ok "backend calls +0 across $replays replays — every response replayed from the idempotency ledger"
  else
    warn "backend calls +$delta (expected +0)"
  fi
}

run_ratelimit() { # rate-limit-burst
  local name="$1"
  require_cmd docker
  local qps total conc body
  qps="$(scenario_field "$name" .qps)"
  total="$(scenario_field "$name" .total)"
  conc="$(scenario_field "$name" .concurrency)"
  body="$(jq -c .body "$SCEN_DIR/$name.json")"

  say "recreating gateway-api with RATE_LIMIT_QPS=$qps (demo default is 1000)"
  DEMO_RATE_LIMIT_QPS="$qps" "${COMPOSE[@]}" up -d gateway-api >/dev/null 2>&1
  wait_ready
  ok "gateway back up with a $qps QPS per-key window"

  say "burst: $total requests, $conc concurrent, one API key"
  local rl0 rl1
  rl0="$(msum "$GATEWAY" 'aegisroute_rate_limited_total')"
  fire_burst "$total" "$conc" "$body" "$TMP/rl.txt"
  status_summary "$TMP/rl.txt"
  rl1="$(msum "$GATEWAY" 'aegisroute_rate_limited_total')"
  local n429
  n429="$(count_field "$TMP/rl.txt" 1 429)"
  if [ "$n429" -gt 0 ]; then
    ok "$n429/$total throttled with 429; aegisroute_rate_limited_total +$((rl1 - rl0))"
  else
    warn "no 429s observed (burst may have spread across windows)"
  fi

  say "restoring the demo default limit"
  "${COMPOSE[@]}" up -d gateway-api >/dev/null 2>&1
  wait_ready
  ok "gateway restored (RATE_LIMIT_QPS=1000)"
}

run_outage() { # backend-outage
  local name="$1"
  require_cmd docker
  local svc total conc probes body
  svc="$(scenario_field "$name" .stop_service)"
  total="$(scenario_field "$name" .total)"
  conc="$(scenario_field "$name" .concurrency)"
  probes="$(scenario_field "$name" .recovery_probes)"
  body="$(jq -c .body "$SCEN_DIR/$name.json")"

  local fast0 cheap0 sc0
  fast0="$(msum "$GATEWAY" 'aegisroute_backend_requests_total{backend="mock-llm-fast"')"
  cheap0="$(msum "$GATEWAY" 'aegisroute_backend_requests_total{backend="mock-llm-cheap"')"
  sc0="$(msum "$GATEWAY" 'aegisroute_circuit_breaker_short_circuits_total')"

  say "stopping $svc (a real outage: connection refused, not a simulated flag)"
  "${COMPOSE[@]}" stop "$svc" >/dev/null 2>&1
  ok "$svc is down"

  say "driving $total requests at $conc concurrent through the outage"
  fire_burst "$total" "$conc" "$body" "$TMP/outage.txt"
  status_summary "$TMP/outage.txt"
  local n200 state fast1 cheap1 sc1
  n200="$(count_field "$TMP/outage.txt" 1 200)"
  state="$(msum "$GATEWAY" 'aegisroute_circuit_breaker_state{backend="mock-llm-fast"')"
  fast1="$(msum "$GATEWAY" 'aegisroute_backend_requests_total{backend="mock-llm-fast"')"
  cheap1="$(msum "$GATEWAY" 'aegisroute_backend_requests_total{backend="mock-llm-cheap"')"
  sc1="$(msum "$GATEWAY" 'aegisroute_circuit_breaker_short_circuits_total')"
  ok "$n200/$total still answered 200 — served by mock-llm-cheap via intra-request failover"
  ok "backend calls during outage: mock-llm-fast +$((fast1 - fast0)), mock-llm-cheap +$((cheap1 - cheap0))"
  ok "breaker on mock-llm-fast: state=$state (0=closed,1=half-open,2=open); short-circuits +$((sc1 - sc0))"

  say "restarting $svc and waiting out the breaker cooldown (CB_COOLDOWN_MS=10000)"
  "${COMPOSE[@]}" start "$svc" >/dev/null 2>&1
  sleep 12
  fire_burst "$probes" 2 "$body" "$TMP/probe.txt"
  local p200 state2
  p200="$(count_field "$TMP/probe.txt" 1 200)"
  state2="$(msum "$GATEWAY" 'aegisroute_circuit_breaker_state{backend="mock-llm-fast"')"
  ok "recovery: $p200/$probes probes 200, breaker state=$state2 (half-open probe succeeded -> closed)"
}

run_batch() { # batch-flood
  local name="$1"
  local items chunk
  items="$(scenario_field "$name" .items)"
  chunk="$(scenario_field "$name" .chunk)"

  local done0
  done0="$(msum "http://localhost:9100" 'aegisroute_batch_items_processed_total')"

  say "submitting $items items in batches of <=$chunk"
  local job_ids=() remaining="$items" offset=0 n jbody jid
  local start="$SECONDS"
  while [ "$remaining" -gt 0 ]; do
    n=$(( remaining > chunk ? chunk : remaining ))
    jbody="$(jq -nc --argjson n "$n" --argjson off "$offset" \
      '{requests: [range($n) | {custom_id: ("demo-item-\($off + .)"), body: {model:"llama-fast", messages:[{role:"user", content:("batch demo \($off + .)")}], temperature:0.9, max_tokens:32}}]}')"
    jid="$(curl -fsS -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
      -d "$jbody" "$GATEWAY/api/v1/batch-jobs" | jq -r '.id')"
    [ -n "$jid" ] && [ "$jid" != "null" ] || fail "batch create returned no id"
    ok "job $jid queued ($n items)"
    job_ids+=("$jid")
    remaining=$(( remaining - n )); offset=$(( offset + n ))
  done

  say "polling ${#job_ids[@]} jobs to a terminal status"
  local jid2 js i
  for jid2 in "${job_ids[@]}"; do
    js=""
    for ((i = 1; i <= 60; i++)); do
      js="$(curl -fsS -H "Authorization: Bearer $API_KEY" "$GATEWAY/api/v1/batch-jobs/$jid2" 2>/dev/null | jq -r '.status' 2>/dev/null || true)"
      case "$js" in succeeded|partially_failed|failed) break ;; esac
      sleep 1
    done
    case "$js" in
      succeeded) ok "job $jid2: succeeded" ;;
      partially_failed|failed) warn "job $jid2: $js" ;;
      *) fail "job $jid2 not terminal after 60s (last: '$js')" ;;
    esac
  done
  local elapsed=$((SECONDS - start)); [ "$elapsed" -lt 1 ] && elapsed=1
  local done1
  done1="$(msum "http://localhost:9100" 'aegisroute_batch_items_processed_total')"
  ok "$items items terminal in ~${elapsed}s (~$(( items * 60 / elapsed )) items/min); worker processed +$((done1 - done0)) items"
}

# --- dispatch ---------------------------------------------------------------

run_scenario() {
  local name="$1"
  local file="$SCEN_DIR/$name.json"
  [ -f "$file" ] || fail "unknown scenario '$name' (try: bash scripts/demo.sh list)"
  local title type
  title="$(jq -r .title "$file")"
  type="$(jq -r .type "$file")"
  printf '\n%s\n' "======================================================================"
  printf 'SCENARIO  %s\n' "$title"
  printf '%s\n' "======================================================================"
  jq -r .narrative "$file" | fold -s -w 78 | sed 's/^/  /'
  printf '\n'
  jq -r '"  watch: " + .watch' "$file" | fold -s -w 78
  case "$type" in
    chat_burst)  run_chat_burst "$name" ;;
    idempotency) run_idempotency "$name" ;;
    ratelimit)   run_ratelimit "$name" ;;
    outage)      run_outage "$name" ;;
    batch)       run_batch "$name" ;;
    *) fail "scenario '$name' has unknown type '$type'" ;;
  esac
  printf '\n  done. dashboard: %s   console: %s\n' "$GRAFANA" "$CONSOLE_URL"
}

list_scenarios() {
  local f
  for f in "$SCEN_DIR"/*.json; do
    printf '  %-20s %s\n' "$(jq -r .name "$f")" "$(jq -r .title "$f")"
  done
}

menu() {
  local files=() f i choice
  for f in "$SCEN_DIR"/*.json; do files+=("$f"); done
  while true; do
    printf '\n%s\n' "--- AegisRoute demo scenarios ------------------------------------"
    i=1
    for f in "${files[@]}"; do
      printf '  %d) %-20s %s\n' "$i" "$(jq -r .name "$f")" "$(jq -r .title "$f")"
      i=$((i + 1))
    done
    printf '  q) quit\n\nDashboards: %s (Grafana)   %s (console)\n' "$GRAFANA" "$CONSOLE_URL"
    printf 'Pick a scenario [1-%d/q]: ' "${#files[@]}"
    read -r choice || return 0
    case "$choice" in
      q|Q) return 0 ;;
      ''|*[!0-9]*) warn "not a number: '$choice'" ;;
      *)
        if [ "$choice" -ge 1 ] && [ "$choice" -le "${#files[@]}" ]; then
          run_scenario "$(jq -r .name "${files[$((choice - 1))]}")"
        else
          warn "out of range: $choice"
        fi ;;
    esac
  done
}

# --- main -------------------------------------------------------------------

require_cmd curl; require_cmd jq
[ -d "$SCEN_DIR" ] || fail "run from the repository root (demo/scenarios/ not found)"

case "${1:-}" in
  list) list_scenarios; exit 0 ;;
esac

wait_ready

# curl >= 7.84 renders %header{...}; older curls print it literally.
CURL_HAS_HEADER_FMT=1
probe="$(curl -s -o /dev/null -w '%header{content-type}' "$GATEWAY/healthz" 2>/dev/null || true)"
case "$probe" in *header*) CURL_HAS_HEADER_FMT=0; warn "curl too old for %header{} — cache-header column disabled" ;; esac

if [ -n "${1:-}" ]; then
  run_scenario "$1"
else
  menu
fi
