# Interactive demo

The MVP ships with an interactive layer so you can *play* with the control
plane, not just read about it: scripted failure/traffic scenarios, a live
Grafana dashboard, and a browser demo console. Everything runs locally with
Docker; nothing here adds a fourth Go binary (Grafana and nginx are external
images, exactly like Prometheus already was).

## One command

```sh
make demo
```

That brings up the demo stack (`docker-compose.yml` + `docker-compose.demo.yml`)
and drops you into a scenario menu. The demo overlay differs from the plain dev
stack in four ways: the mock backends simulate ~40ms of inference latency (so
cache/failover effects are visible to the eye), `RATE_LIMIT_QPS` is raised to
1000 (so burst scenarios exercise the right mechanism), and it adds **Grafana**
and the **demo console**:

| URL | What |
| --- | --- |
| http://localhost:3000 | Grafana — auto-provisioned "AegisRoute — Control Plane" dashboard, anonymous read-only |
| http://localhost:8000 | Demo console — fire scenarios from the browser, live stat tiles |
| http://localhost:8080 | The gateway itself |
| http://localhost:9090 | Raw Prometheus |

Best experience: Grafana on one half of the screen, the console or a terminal
on the other.

## The scenarios

Declarative scenario files live in `demo/scenarios/*.json`; `scripts/demo.sh`
reads and fires them, then reports what the control plane did (status
distribution, cache headers, exact metric deltas read from `/metrics`).

```sh
bash scripts/demo.sh                    # interactive menu
bash scripts/demo.sh list               # list scenarios
bash scripts/demo.sh cache-storm        # run one
```

| Scenario | What it proves |
| --- | --- |
| `cache-storm` | 200 identical requests, 40-way concurrent: first wave MISSes into the ~40ms backend, the rest are ~1ms Redis HITs. Times one MISS and one HIT so the speedup is a number. |
| `idempotency-replay` | 10 requests race one fresh `Idempotency-Key`: exactly 1 executes, 9 get 409. Then 20 replays of the completed key — backend-call delta is **+0**. |
| `rate-limit-burst` | Recreates the gateway with `RATE_LIMIT_QPS=10`, fires 50 requests, counts the 429s, restores the default. |
| `backend-outage` | **The flagship.** Stops the `mock-llm-fast` container (a real outage), drives 150 requests: breaker trips after 5 failures, selector short-circuits the corpse, every request still answers 200 via failover to `mock-llm-cheap`. Then restarts it and shows the breaker close again after the 10s cooldown. |
| `batch-flood` | 200 items as two async batch jobs through the transactional outbox → Redis Stream → bounded worker pool; polls to terminal and reports items/min. |

Requirements: `curl` (7.84+ for the cache-header column), `jq`; `docker` only
for the two scenarios that stop/recreate a compose service.

## The demo console (:8000)

A single static page served by nginx, which also reverse-proxies the gateway
and Prometheus so the browser stays same-origin (the gateway itself stays
CORS-free). Buttons fire the browser-runnable scenarios (cache storm,
idempotency race, batch job, a steady live-traffic driver); stat tiles poll
Prometheus every 3s, including per-backend breaker state.

The intended party trick: turn on **Live traffic**, then in a terminal run
`docker compose stop mock-llm-fast` and watch the breaker tile flip to OPEN
while the request counter keeps climbing — then `docker compose start
mock-llm-fast` and watch it close.

## The GIF

`docs/assets/demo.gif` is recorded with [vhs](https://github.com/charmbracelet/vhs)
from `scripts/demo.tape`. To re-record: bring the demo stack up, then

```sh
make demo-gif        # requires: brew install vhs
```

## Grafana provisioning layout

```
observability/grafana/
  provisioning/datasources/datasource.yml   # Prometheus at prometheus:9090, uid aegisroute-prom
  provisioning/dashboards/provider.yml      # loads /var/lib/grafana/dashboards
  dashboards/aegisroute.json                # the dashboard (13 panels over the 15-metric set)
```

The dashboard is file-provisioned and read-only-anonymous; edit the JSON (or
edit in the UI as `admin` and export) and Grafana re-reads it within 30s.

## Hosting a public demo (optional, not automated)

The stack is self-contained, so a throwaway public instance is one small VM
away: `docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d`
behind a reverse proxy. If you do this: keep the seeded demo key (it's
harmless — the backends are fakes), leave `DEMO_RATE_LIMIT_QPS` at a low value
instead of 1000 to bound abuse, put Grafana behind the same proxy, and add a
nightly `docker compose down -v && up -d` cron to reset state. Costs and TLS
are the only real work; nothing in the repo needs to change.
