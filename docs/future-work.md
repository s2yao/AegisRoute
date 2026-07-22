# Future work (post-MVP)

Everything below is **out of scope for the MVP** and intentionally not built.
Each item has a clear seam in the current design where it would slot in without
a rewrite. Nothing here is a bug or a TODO in the runtime path — the MVP is
complete and self-consistent as shipped.

## Real model providers

Swap the deterministic `mock-llm` backends for real OpenAI-compatible providers.
The seam already exists: `internal/inference.Client.Do(ctx, backend, body)` is
the single outbound call, and backends are rows with a `base_url` and `kind`.
Real providers would add per-provider auth headers and (optionally) a
provider-specific `kind` implementation behind the same `InferenceDoer`
interface. No handler or worker changes.

## SSE / streaming completions

The gateway deliberately rejects `stream:true` today (`400
unsupported_streaming`). Streaming would add a passthrough path that proxies the
provider's `text/event-stream` to the client. It interacts with caching
(streamed responses are not cache-eligible as-is) and with the response cap, so
it is a feature in its own right, not a small patch.

## OIDC + RBAC for the control plane

Admin routes use a single static admin token compared in constant time. A real
deployment would put OIDC/JWT in front and RBAC on the admin operations
(who may create backends vs read jobs). The middleware chain and
consumer-declared auth interfaces are the insertion point.

## Consumer-group lag & queue observability metrics

The worker exposes per-item and worker-failure counters, but not Redis Stream
consumer-group depth / lag or outbox backlog age. Adding gauges for
`XPENDING`-derived lag and pending-outbox age would make the async path
operable at scale. (Note: `XAUTOCLAIM` is already used in the MVP for stream
recovery — this item is about *observing* the queue, not recovering it.)

## Global / distributed concurrency control

`max_in_flight` and the circuit breaker are **per-process** by design: N
replicas each admit up to `N × max_in_flight` and keep independent breaker
state. Making these global (a shared token bucket / distributed breaker in
Redis) is the largest single piece of future work and was an explicit non-goal
for the MVP.

## Infrastructure & tooling

- **Kubernetes** manifests / Helm chart (the MVP ships Docker Compose only).
- **Terraform** for managed Postgres/Redis and deployment.
- **gRPC** surface alongside the HTTP API.
- **sqlc** to generate typed query code (the MVP uses hand-written pgx SQL by
  choice, for full control over every query).

## Load & dashboards

- **k6** load-test scripts for the sync and batch paths. (`make bench` covers
  single-machine load with `hey`, and `make demo` ships scripted traffic
  scenarios — k6 would add scriptable, assertable load profiles.)
- ~~Grafana dashboards~~ — **shipped** post-MVP: the demo stack provisions a
  Grafana dashboard over the full metric set (see `docs/demo.md`).

These would validate and visualize behavior under load but add no new
control-plane capability.

## Hosted public demo

The interactive demo (`docs/demo.md`) runs locally. Hosting a throwaway public
instance (small VM, reverse proxy + TLS, low `DEMO_RATE_LIMIT_QPS`, nightly
state reset) needs no repo changes — it is an ops task, deliberately left out.
