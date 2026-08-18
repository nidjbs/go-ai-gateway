# light-llm-gateway

A lightweight, OpenAI-compatible AI gateway for routing model aliases across OpenAI-compatible and Anthropic upstreams. It supports retries, failover, API-key tenancy, quotas, Redis-backed distributed limits, usage records, metrics, and optional prompt-injection guardrails.

## Quick Start

Requirements: Go 1.22+ and an upstream compatible with the configured provider.

```bash
cp configs/config.example.yaml configs/config.yaml
export UPSTREAM_API_KEY=replace-with-upstream-key
go run ./cmd/gateway --config configs/config.yaml
```

The sample listens on `http://127.0.0.1:8080`; operational endpoints listen on `http://127.0.0.1:8081`.

```bash
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

`configs/config.example.yaml` defaults to `auth.mode: none` for local development. Set `static` or `api-key` before exposing the gateway outside a trusted network.

## Docker

The Compose setup starts two gateway replicas, Redis, and a mock upstream. It is intended as a distributed-mode smoke test.

```bash
docker compose up --build
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-smoke-test-key' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

Edit `configs/config.docker.yaml` to use a real upstream, then recreate the gateway containers:

```bash
docker compose up -d --force-recreate
```

## Features

| Feature | Description |
|---|---|
| Model aliases | Route a client-visible model name to one or more upstream models |
| Retry and failover | Retry eligible errors and move to lower-priority providers |
| Provider adapters | OpenAI-compatible upstreams and Anthropic Messages API |
| Authentication | Local unauthenticated mode, static Bearer token, or per-team API keys |
| Limits and quotas | Per-key rate limits plus daily, monthly, and per-alias token quotas |
| Distributed state | Redis-backed rate limits, quotas, and guardrail tracking across replicas |
| Guardrails | Optional prompt-injection scanning and per-key penalty tracking |
| Observability | Prometheus metrics, OpenTelemetry tracing, structured usage records |

## Configuration

Start with `configs/config.example.yaml`. The essential fields are:

```yaml
providers:
  primary:
    type: openai
    base_url: https://api.example.com/v1
    api_key_env: UPSTREAM_API_KEY

aliases:
  chat:
    provider: primary
    model: upstream-model-name
```

Clients always use the OpenAI-compatible endpoints. An alias can select a failover chain:

```yaml
aliases:
  chat:
    providers:
      - {name: primary, model: model-a, priority: 0}
      - {name: backup, model: model-b, priority: 1}
```

For multiple replicas, configure the Redis drivers shown in `configs/config.docker.yaml`. With no driver configured, rate limits, quotas, and guardrail tracking use process-local memory.

## Authentication and Limits

`auth.mode` supports:

- `none`: no authentication; local development only.
- `static`: one Bearer token read from an environment variable.
- `api-key`: per-team Bearer API keys with individual limits and quotas.

Static-token configuration:

```yaml
auth:
  mode: static
  token_env: GATEWAY_STATIC_TOKEN
```

API-key configuration:

```yaml
auth:
  mode: api-key
teams:
  - id: team-frontend
    name: Frontend
    api_keys:
      - id: key-prod
        key: sk-replace-me-with-a-generated-key
        limits:
          rps: 20
          burst: 40
          preday_tokens: 5000000
```

Generate a new key with either `go run ./cmd/keygen` or `./scripts/genkey.sh`.

## Operations

| Endpoint | Purpose |
|---|---|
| `GET /healthz`, `GET /livez` | Liveness |
| `GET /readyz` | Readiness after the configured startup window |
| `GET /metrics` | Prometheus metrics |
| `GET /v1/models` | Available aliases |
| `POST /v1/chat/completions` | OpenAI-compatible chat completions, including streaming |
| `POST /v1/embeddings` | OpenAI-compatible embeddings |

## Documentation

- [Architecture](docs/architecture.md): request pipeline, provider behavior, Redis distributed state, guardrails, and observability.
- [Extending](docs/extending.md): custom authentication, usage sinks, and storage drivers.
- [Configuration example](configs/config.example.yaml): all supported built-in settings.
- [Security policy](SECURITY.md): vulnerability reporting process.
- [Contributing](CONTRIBUTING.md): local contribution workflow.

## Development

```bash
gofmt -w $(find . -name '*.go')
go test ./...
go vet ./...
go build ./cmd/gateway
scripts/e2e.sh
scripts/e2e_apikey.sh
```

The E2E scripts run against bundled mock upstreams and require Go, Bash, curl, and Python 3.
