# notification-dispatcher

[![CI](https://github.com/jiin-yang/notification-dispatcher/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/jiin-yang/notification-dispatcher/actions/workflows/ci.yml)

Event-driven notification dispatcher. The API accepts single or batch
notification requests and publishes them to RabbitMQ. A worker pool consumes
per-channel priority queues and delivers each message via an HTTP webhook,
with retries, dead-letter handling, per-channel circuit breaking, and ingress
rate limiting.

- **Language:** Go 1.25
- **Transport:** RabbitMQ (topic exchange, manual ack, publisher confirms)
- **Storage:** PostgreSQL (pgx/v5, goose migrations)
- **Observability:** Prometheus `/metrics`, Kubernetes-style health probes,
  structured JSON logs with correlation IDs

## Table of contents

1. [Quickstart](#quickstart)
2. [Architecture](#architecture)
3. [HTTP API](#http-api)
4. [Configuration](#configuration)
5. [Running on the host](#running-on-the-host)
6. [Observability](#observability)
7. [Testing](#testing)
8. [Project layout](#project-layout)

## Quickstart

Prerequisites: Docker with compose plugin.

```bash
# Full stack: postgres + rabbitmq + webhook echo + api + worker
make app-up

# Send a notification
curl -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -d '{"to":"+905551234567","channel":"sms","content":"hello","priority":"normal"}'

# Watch the webhook echo
docker compose --profile app logs -f webhook

# Stop
make app-down
```

Swagger UI is available at http://localhost:8080/docs.
Worker metrics at http://localhost:9090/metrics.

## Architecture

```
 client
    |
    v
 +------+           +--------------------+       +------------------+
 | api  | -------> | RabbitMQ topic     | ----> | worker pool      |
 | HTTP |  publish | notifications.topic|  ack  | 9 queues         |
 +------+          +--------------------+       | channel.priority |
    |                       |                   +---------+--------+
    |                       |  retry / DLQ                |
    v                       v                             v
 +------------+      +--------------+              +-------------+
 | PostgreSQL |      | retry + DLQ  |              | webhook HTTP|
 | notifs /   |      | exchanges    |              | delivery    |
 | batches /  |      | (TTL loops)  |              +-------------+
 | processed /|      +--------------+
 | attempts   |
 +------------+
```

Key building blocks:

- **Per-channel-and-priority queues.** Nine queues (`email.high`, `email.normal`,
  …, `push.low`) are bound to the same topic exchange using routing keys of the
  form `{channel}.{priority}`. High-priority traffic is never blocked by a
  backlog on a different channel.
- **Retry via TTL + DLX.** Each channel has three retry queues
  (`<channel>.retry.L1/L2/L3`) with increasing TTLs. On a recoverable failure
  the worker republishes the message to the retry exchange at the next level;
  when the TTL expires RabbitMQ dead-letters the message back onto the main
  exchange. After exhausting levels (default 3 attempts) the message is routed
  to a per-channel DLQ.
- **Circuit breaker.** One `sony/gobreaker` instance per channel. An open
  breaker short-circuits provider calls and routes messages straight to retry
  or DLQ without touching the webhook.
- **Rate limiting.** An in-process token bucket per channel caps ingress into
  the API. Rejections surface as `429` with a `Retry-After` header.
- **Idempotency.** The `Idempotency-Key` request header is persisted on a
  unique partial index; replays return the original notification instead of
  creating a duplicate.
- **Consumer idempotency.** `processed_messages` records every handled
  message_id so redeliveries (after a broker-level requeue) don't double-send.
- **Hexagonal layout.** `domain` defines the core types, `app` orchestrates
  the use cases against ports (`Repository`, `MessagePublisher`,
  `RetryPublisher`, …), and each `adapter/*` package implements one port
  against a concrete technology.

## HTTP API

The full machine-readable spec lives at [`internal/adapter/http/openapi.yaml`](internal/adapter/http/openapi.yaml)
and is served live at `GET /docs/openapi.yaml`. A Swagger UI viewer is mounted at `GET /docs`.

| Method | Path                                  | Purpose                                 |
|--------|---------------------------------------|-----------------------------------------|
| POST   | `/notifications`                      | Enqueue one (supports `Idempotency-Key`)|
| GET    | `/notifications`                      | List with filters & pagination          |
| GET    | `/notifications/{messageId}`          | Get a single notification               |
| DELETE | `/notifications/{messageId}`          | Cancel (only if `pending`)              |
| POST   | `/notifications/batch`                | Enqueue up to 1000 in one request       |
| GET    | `/notifications/batch/{batchId}`      | Batch status summary (counts per state) |
| GET    | `/admin/dlq/{channel}`                | Inspect DLQ depth                        |
| POST   | `/admin/dlq/{channel}/replay?limit=N` | Replay DLQ messages back to main queues |
| GET    | `/health`                             | Alias for `/health/ready`               |
| GET    | `/health/live`                        | Liveness probe                          |
| GET    | `/health/ready`                       | Readiness probe (pings DB + RMQ)         |
| GET    | `/metrics`                            | Prometheus metrics                      |
| GET    | `/docs`                               | Swagger UI                              |

`X-Correlation-ID` is honored on every request and echoed on the response. A
new `X-Request-ID` is generated and returned as well. Both are included on
every log line.

## Configuration

All settings come from environment variables. `.env` in the repo root is
picked up by the Makefile for local host-mode runs.

| Variable                  | Default | Description                                       |
|---------------------------|---------|---------------------------------------------------|
| `HTTP_PORT`               | `8080`  | API listen port                                   |
| `METRICS_PORT`            | `9090`  | Worker metrics + liveness port                    |
| `DB_DSN`                  | —       | Postgres DSN (required)                           |
| `RMQ_URL`                 | —       | RabbitMQ URL (required)                           |
| `WEBHOOK_URL`             | —       | Webhook target for all channels (required)        |
| `WEBHOOK_TIMEOUT`         | `10s`   | Per-request timeout                               |
| `LOG_LEVEL`               | `info`  | `debug` / `info` / `warn` / `error`               |
| `SHUTDOWN_TIMEOUT`        | `30s`   | Graceful shutdown budget                          |
| `RATE_LIMIT_PER_SECOND`   | `100`   | Ingress tokens/sec per channel                    |
| `RATE_LIMIT_BURST`        | `200`   | Bucket burst capacity per channel                 |
| `CB_FAILURE_THRESHOLD`    | `5`     | Consecutive failures before opening the breaker   |
| `CB_OPEN_DURATION`        | `30s`   | How long the breaker stays open                   |
| `CB_HALF_OPEN_MAX_CALLS`  | `2`     | Max calls while half-open                         |

Copy [.env.example](.env.example) to `.env` for a starting point (if present);
otherwise export the three required variables directly.

## Running on the host

```bash
make hooks-install   # one-time: activates pre-push test gate
make dev-up          # start postgres + rabbitmq containers
make migrate-up      # apply all migrations
make run-api         # terminal 1
make run-worker      # terminal 2
```

## Observability

- `/metrics` on both api (`:8080`) and worker (`:9090`). Every counter is
  labeled with `service` so the two processes share a dashboard.
- Notable collectors: `http_requests_total`, `http_request_duration_seconds`,
  `notifications_enqueued_total`, `notifications_delivered_total`,
  `notifications_retried_total`, `notifications_dlq_total`,
  `rate_limit_rejected_total`, `circuit_breaker_state`,
  `notifications_in_flight`.
- JSON logs on stdout via `log/slog`. Every request log carries
  `correlation_id` and `request_id`; worker logs carry `notification_id`,
  `channel`, and `attempt`.
- Kubernetes-style probes: `/health/live` never touches dependencies so a
  restart loop driven by a flaky DB is impossible; `/health/ready` pings
  Postgres and RabbitMQ with a 2s deadline.

## Testing

- `make test-unit` — pure unit tests, no external services.
- `make test-e2e` — spins up `notifications_test`, applies migrations,
  runs the `./test/e2e` suite against real Postgres + RabbitMQ.
- `make test-all` — both. Gated on every push via `.githooks/pre-push`.

## Continuous Integration

Every push to `main` and every pull request runs four parallel jobs:

- **lint** — `golangci-lint` (govet, staticcheck, errcheck, ineffassign,
  unused, misspell, revive) via `.golangci.yml`.
- **test-unit** — `go test ./...` with Go module and build cache.
- **test-e2e** — GitHub Actions service containers (postgres:16 +
  rabbitmq:3.13) run the full `./test/e2e` suite with real infra.
- **docker-build** — BuildKit matrix build of `Dockerfile` (api + worker)
  and `Dockerfile.migrate`, with GHA layer cache.

Tagging `vX.Y.Z` triggers `.github/workflows/release.yml` which builds and
pushes multi-arch images (linux/amd64 + linux/arm64) for api, worker, and
migrate to GHCR (`ghcr.io/jiin-yang/notification-dispatcher-*`) with
provenance and SBOM attestations, then creates a GitHub Release with
auto-generated notes.

A weekly security scan (`.github/workflows/security.yml`) runs CodeQL for
Go and a Trivy filesystem scan for HIGH/CRITICAL CVEs; findings are uploaded
to GitHub Code Scanning.

The e2e harness uses a `test_` queue prefix and a dedicated database so
parallel dev runs and e2e runs never collide.

## Project layout

```
cmd/
  api/         # HTTP API entry point
  worker/      # Worker entry point (metrics + delivery)
internal/
  adapter/
    http/      # chi router, handlers, middlewares, OpenAPI spec
    postgres/  # pgx repositories + goose migrations helpers
    rabbitmq/  # topology, consumer, publishers, retry adapter
    provider/  # webhook provider, registry, circuit breaker
  app/         # use cases (ports defined here, no external imports)
  domain/      # pure domain types + validation
  config/      # env parsing
  platform/
    logger/    # slog builder
    metrics/   # prometheus collectors
    ratelimit/ # per-channel token bucket
migrations/    # goose SQL files
test/e2e/      # integration test suite
```
