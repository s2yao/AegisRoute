# API reference

All responses carry an `X-Request-ID` header (echoed from the request or newly
generated). Errors always use one envelope:

```json
{ "error": { "code": "...", "message": "...", "request_id": "..." } }
```

`code` is one of: `unauthorized`, `bad_request`, `not_found`, `conflict`,
`rate_limited`, `unsupported_streaming`, `internal`, `upstream_unavailable`.

## Authentication

| Route group | Auth | Header |
| --- | --- | --- |
| `/healthz`, `/readyz`, `/metrics` | none | — |
| `/v1/*`, `/api/v1/batch-jobs*` | tenant API key | `Authorization: Bearer <key>` |
| `/api/v1/backends*`, `/api/v1/routing-policies*` | admin token | `X-Admin-Token: <token>` |

Credentials in query parameters are rejected with `400` before auth runs.
Demo values (LOCAL-ONLY): key `sg_dev_key_123`, admin token `dev_admin_token`.

## Health & metrics

- `GET /healthz` → `200 {"status":"ok"}` (liveness; checks no dependencies).
- `GET /readyz` → `200 {"status":"ready"}` when Postgres and Redis both answer a
  ping, else `503`.
- `GET /metrics` → Prometheus exposition of the `aegisroute_*` collectors.
  `control-worker` exposes `/healthz` + `/metrics` on `:9100`.

## Models

`GET /v1/models` (bearer) → OpenAI-style list of logical models served by
enabled backends, de-duplicated by model name:

```json
{ "object": "list", "data": [ { "id": "llama-fast", "object": "model", "owned_by": "aegisroute" } ] }
```

## Chat completions

`POST /v1/chat/completions` (bearer). Request:

```json
{ "model": "llama-fast",
  "messages": [ {"role": "user", "content": "..."} ],
  "temperature": 0, "max_tokens": 32, "stop": ["\n"] }
```

- `model` and a non-empty `messages` array are required; roles are
  `system|user|assistant` with non-empty string content.
- `temperature` ∈ [0, 2] and `max_tokens` > 0 when present; unknown fields and
  case variants are rejected (strict). `stream:true` → `400 unsupported_streaming`.
- Body cap 1 MiB (`413` over it).

Response is the backend's completion body verbatim, plus headers:

| Header | Meaning |
| --- | --- |
| `X-AegisRoute-Backend` | backend that served it (absent on a cache hit) |
| `X-AegisRoute-Routing-Policy` | policy that chose it |
| `X-AegisRoute-Cache` | `HIT` \| `MISS` \| `BYPASS` |

Optional `Idempotency-Key` header (≤ 255 chars): a completed same-key request
replays the stored response; the same key with a different body → `409`; an
in-progress key → `409`. See [design-decisions.md](design-decisions.md).

Status mapping: unknown model → `404`; no capacity / all backends unavailable →
`503`; permanent upstream error → `502`; over rate limit → `429`.

## Batch jobs

`POST /api/v1/batch-jobs` (bearer; `Idempotency-Key` supported; body cap 10 MiB):

```json
{ "requests": [
  { "custom_id": "req-1",
    "body": { "model": "llama-fast",
      "messages": [ {"role": "user", "content": "..."} ],
      "temperature": 0, "max_tokens": 32 } }
] }
```

Rules: 1..100 requests; each `custom_id` required, non-empty, unique within the
batch; each `body` passes the same chat validation; **all items must use the
same model** (stored in `batch_jobs.model`). Response (`201`):

```json
{ "id": "<uuid>", "object": "batch_job", "status": "queued",
  "total_items": 1, "completed_items": 0, "failed_items": 0 }
```

- `GET /api/v1/batch-jobs` → this tenant's jobs, newest first.
- `GET /api/v1/batch-jobs/{id}` → one job (status, model, counters, timestamps).
- `GET /api/v1/batch-jobs/{id}/items` → the job's items (`custom_id`, `status`,
  `attempts`, echoed `request`, and `response`/`error` once terminal).

All three are tenant-scoped: another tenant's job id returns `404` — existence is
never leaked across tenants. Job status: `queued → running → {succeeded |
partially_failed | failed}` (all items succeeded → succeeded; mixed →
partially_failed; all failed → failed). Item status: `queued → running →
{succeeded | failed}`.

## Admin control plane

Admin-token routes (`X-Admin-Token`) manage backends and routing policies:

- `GET/POST /api/v1/backends`, `PATCH /api/v1/backends/{id}` — create/list/patch
  model backends. Patch omits immutable fields (`name`, `model_name`, `kind`);
  disable is soft (`enabled=false`), never a hard delete.
- `GET/POST /api/v1/routing-policies`, `PATCH /api/v1/routing-policies/{id}` —
  manage routing policies (strategy `priority_weighted`).

Duplicate-name creates return `409 conflict`.
