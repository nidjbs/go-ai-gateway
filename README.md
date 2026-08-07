# light-llm-gateway

A lightweight OpenAI-compatible LLM gateway with model aliases, retry, and provider failover.

It is intentionally small: running the gateway requires Go and an OpenAI-compatible upstream only. PostgreSQL, Kafka, and ClickHouse are not runtime dependencies.

## Quick Start

```bash
cp configs/config.example.yaml configs/config.yaml
export UPSTREAM_API_KEY=replace-with-upstream-key
go run ./cmd/gateway --config configs/config.yaml
```

The sample configuration serves the API at `http://127.0.0.1:8080` and operational endpoints at `http://127.0.0.1:8081`.

- `GET /healthz` and `GET /livez` return process liveness.
- `GET /readyz` returns `204` when the startup readiness window has elapsed.
- `GET /metrics` exposes Prometheus metrics, including SIU-compatible LLM request latency, TTFT, output-token pacing, and token-usage histograms.

```bash
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

## Authentication

Authentication defaults to `none`, which is appropriate only for local use or when an outer proxy authenticates callers. To enable a static Bearer token:

```yaml
auth:
  mode: static
  token_env: GATEWAY_STATIC_TOKEN
```

```bash
export GATEWAY_STATIC_TOKEN=replace-with-gateway-token
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $GATEWAY_STATIC_TOKEN"
```

## Routing and Failover

Clients send aliases such as `chat`; aliases resolve to configured upstream models. An alias can use a priority-ordered provider chain:

```yaml
aliases:
  chat:
    providers:
      - {name: primary, model: model-a, priority: 0}
      - {name: backup, model: model-b, priority: 1}
```

The gateway retries eligible failures against the current provider before failing over. It does not retry after an SSE response has emitted data, so a streamed response is never mixed between providers.

## API Surface

- `GET /healthz`
- `GET /livez`
- `GET /readyz` (operational listener)
- `GET /metrics` (operational listener)
- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/embeddings`

## Development

```bash
gofmt -w $(find . -name '*.go')
go test ./...
go vet ./...
go build ./cmd/gateway
scripts/e2e.sh
```

`scripts/e2e.sh` builds the gateway and local mock providers, then runs an end-to-end agent flow without a real model or credentials. It requires Go, Bash, curl, and Python 3; it covers static authentication, multi-turn chat, tool-call continuation, streaming, retry/failover, embeddings, and validation errors.

See [architecture](docs/architecture.md) and [extending](docs/extending.md) for component and extension boundaries.
