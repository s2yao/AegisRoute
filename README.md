# AegisRoute
OpenAI-compatible LLM inference gateway in Go with multi-backend routing, streaming responses, async batch processing, quota control, and Prometheus observability.

AegisRoute is a backend infrastructure project for managing LLM API traffic. It sits between clients and model providers, handling routing, reliability, batching, usage control, and metrics around inference requests.

## Features

* OpenAI-compatible `/v1/chat/completions` API
* Streaming chat completions with Server-Sent Events
* Multi-backend model routing through a provider abstraction
* Circuit breakers, timeouts, retries, and fallback handling
* Async batch inference pipeline using Redis Streams
* PostgreSQL-backed request logs, API keys, idempotency records, and usage data
* Per-key rate limits, quotas, and model access policies
* Prometheus metrics for latency, failures, token usage, queue depth, and backend health
* Local mock LLM provider for testing routing, streaming, and failure scenarios

## Architecture

```text
Client
  |
  v
gateway-api  --->  PostgreSQL
  |                 API keys, requests, usage, jobs
  |
  +----> Redis
  |      cache, rate limits, idempotency, streams queue
  |
  +----> mock-llm / model backends
  |
  v
Prometheus metrics


control-worker
  |
  +----> Redis Streams
  +----> model backends
  +----> PostgreSQL
```

## Services

| Service          | Description                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------- |
| `gateway-api`    | Public API for completions, streaming, auth, quota checks, routing, caching, and batch submission |
| `control-worker` | Background worker for async inference jobs from Redis Streams                                     |
| `mock-llm`       | Local mock model backend for testing latency, streaming, and failures                             |

## Tech Stack

* Go
* PostgreSQL
* Redis Streams
* Docker Compose
* Prometheus
* Make
* GitHub Actions

## Quick Start

```bash
make up
make migrate-up
make test
make e2e
```

Send a chat completion request:

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer dev-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-001" \
  -d '{
    "model": "mock-fast",
    "messages": [
      {
        "role": "user",
        "content": "Explain token-aware routing in one paragraph."
      }
    ],
    "stream": false
  }'
```

## Batch Inference

```bash
curl -X POST http://localhost:8080/v1/batches \
  -H "Authorization: Bearer dev-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mock-batch",
    "requests": [
      {
        "custom_id": "req-1",
        "messages": [
          {
            "role": "user",
            "content": "Explain circuit breakers."
          }
        ]
      },
      {
        "custom_id": "req-2",
        "messages": [
          {
            "role": "user",
            "content": "Explain retry backoff."
          }
        ]
      }
    ]
  }'
```

Check batch status:

```bash
curl http://localhost:8080/v1/batches/<batch_id> \
  -H "Authorization: Bearer dev-api-key"
```

## Metrics

Prometheus metrics are exposed at:

```text
/metrics
```

Tracked signals include:

* request count
* latency
* backend failures
* circuit breaker state
* token usage
* estimated cost
* cache hit rate
* queue depth
* batch job success/failure count

## Project Goals

AegisRoute is built to demonstrate production-style backend and AI infrastructure concepts:

* API gateway design
* LLM provider abstraction
* streaming HTTP responses
* Redis Streams consumer groups
* bounded worker pools
* idempotent async processing
* PostgreSQL schema design
* rate limiting and quota enforcement
* circuit breaker reliability patterns
* Prometheus observability
* Dockerized local development
* CI-backed integration tests

## Non-Goals

AegisRoute does not train or fine-tune models. The focus is the infrastructure layer around LLM inference traffic.

## License

MIT
