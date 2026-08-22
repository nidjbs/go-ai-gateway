# go-ai-gateway

A lightweight, OpenAI-compatible AI gateway for routing model aliases across OpenAI-compatible and Anthropic upstreams. It supports retries, failover, API-key tenancy, quotas, Redis-backed distributed limits, usage records, metrics, and optional prompt-injection guardrails.

## Quick Start

### Option A: Docker (recommended)

The Docker path starts two gateway replicas, Redis, and a bundled mock upstream. It does not require a local Redis instance or a real upstream API key.

```bash
docker compose up --build -d
```

Wait about 5 seconds for the configured readiness window, then verify the gateway:

```bash
# Liveness: expect HTTP 200.
curl http://127.0.0.1:8081/healthz

# Readiness: expect HTTP 204 after the 5-second startup window.
curl -o /dev/null -s -w '%{http_code}\n' http://127.0.0.1:8081/readyz

# Model list: the Docker configuration requires this API key.
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer sk-smoke-test-key'

# Chat completion: expect an OpenAI-compatible JSON response.
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-smoke-test-key' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

`sk-smoke-test-key` is the API key configured in `configs/config.docker.yaml`. Requests without the key return `401` in this Docker setup. To use a real upstream, edit that file and recreate the gateway containers:

```bash
docker compose up -d --force-recreate
```

Stop the local stack when finished:

```bash
docker compose down
```

### Option B: Local Go development

Requirements: Go 1.26+, Redis on `127.0.0.1:6379`, and an upstream compatible with the configured provider. The example configuration enables Redis drivers and points the provider at `host.docker.internal:19090`, so update `base_url` to a reachable real upstream or to a local mock before starting. `host.docker.internal` is not available on every host.

For the bundled mock upstream, run it in a separate terminal:

```bash
go run ./scripts/mock_upstream.go --listen 127.0.0.1:19090 --role primary
```

Then copy the configuration, change the provider `base_url` to `http://127.0.0.1:19090/v1`, and start the gateway:

```bash
cp configs/config.example.yaml configs/config.yaml
export UPSTREAM_API_KEY=replace-with-upstream-key
go run ./cmd/gateway --config configs/config.yaml
```

The example configuration defaults to `auth.mode: none`, so local requests do not need an Authorization header:

```bash
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

The API listens on `http://127.0.0.1:8080`; operational endpoints listen on `http://127.0.0.1:8081`. Set `auth.mode` to `static` or `api-key` before exposing the gateway outside a trusted network.

## Docker

The Compose setup is a distributed-mode smoke test. It runs two gateway replicas with shared Redis-backed rate-limit and quota state. Gateway 1 exposes API/operational ports `8080`/`8081`; gateway 2 exposes `8082`/`8083`. See Option A above for the complete verification flow.

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

For production, configure **hashes** instead of plaintext keys — the gateway
authenticates against SHA-256 digests, so the plaintext never needs to live in
config files or process memory:

```bash
go run ./cmd/keygen -sha256   # prints sha256:<hex> for the config file
```

Rotation is a config change + restart: add the new digest, restart, remove the old one.

## Operations

| Endpoint | Purpose |
|---|---|
| `GET /healthz`, `GET /livez` | Liveness |
| `GET /readyz` | Dependency-aware readiness: 204 only after the startup window **and** when every configured backend (e.g. Redis) responds to its probe |
| `GET /version` | Build metadata (version / commit / build date) |
| `GET /metrics` | Prometheus metrics |
| `GET /v1/models` | Available aliases |
| `POST /v1/chat/completions` | OpenAI-compatible chat completions, including streaming |
| `POST /v1/responses` | OpenAI Responses API passthrough, including streaming (OpenAI-compatible providers) |
| `POST /v1/embeddings` | OpenAI-compatible embeddings |

`/healthz` and `/livez` return `200`. `/readyz` returns `503` during the startup window or while a dependency probe fails, then `204`: 5 seconds in `configs/config.docker.yaml` and 10 seconds in `configs/config.example.yaml`. Set `ops_token_env` to require a bearer token on operational endpoints.

## Production hardening

- **Dependency-aware readiness & fail-fast startup** — a replica whose Redis is unreachable reports 503, and the process refuses to start with a misconfigured backend.
- **Quota that cannot be bypassed** — per-key `max_requests_per_day` counters, estimated token charging when upstreams omit usage or a stream ends before a usage chunk (failure, timeout, client abort), and per-request ceilings via Streaming chat requests automatically inject stream_options.include_usage=true (per-provider force_usage can force or suppress it) so token data comes from the upstream whenever possible, `max_tokens_per_request`.
- **Stream timeouts** — `server.stream_idle_timeout` / `server.stream_max_duration` bound silent and endless streams, ending them with an SSE error frame.
- **Idempotent retries** — with `server.idempotency_enabled: true`, clients sending an `Idempotency-Key` on non-streaming chat/responses get the cached response on retry instead of a second upstream execution and second charge.
- **HTTP timeouts** — `server.read_timeout` / `server.idle_timeout` (defaults 30s/90s); `WriteTimeout` is intentionally unset so streams can run indefinitely.
- **Ops auth** — `ops_token_env` protects `/metrics`, `/readyz`, `/version`.
- **Hashed API keys** — `go run ./cmd/keygen -sha256` for digest-based key config and clean rotation.
- **Guardrails escape hatch** — `guardrails.allowlist` bypasses scanning for false-positive-prone payloads in block mode.
- **Circuit breaker** — enabled in the example configs; trips on consecutive upstream failures.

See [Operations guide](docs/operations.md) for the multi-replica runbook, load testing (`scripts/load_test.sh`), and alerting. Reference Kubernetes manifests live in `deploy/k8s.yaml`.

## Documentation

- [Architecture](docs/architecture.md): request pipeline, provider behavior, Redis distributed state, guardrails, and observability.
- [Extending](docs/extending.md): custom authentication, usage sinks, and storage drivers.
- [Operations](docs/operations.md): multi-replica production runbook, probes, scaling, secrets, and alerting.
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

The E2E scripts run against bundled mock upstreams and require Go, Bash, curl, and Python 3. `scripts/e2e.sh` covers routing, retries, failover, and static authentication; `scripts/e2e_apikey.sh` covers API-key authentication, rate limits, and quotas.
