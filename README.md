# light-llm-gateway

A lightweight OpenAI-compatible LLM gateway with model aliases, retry, provider failover, and protocol adapters.

Clients always use the OpenAI API surface. Upstream providers can use either `openai` (OpenAI-compatible endpoints) or `anthropic` (Messages API); the gateway translates the supported chat subset for Anthropic.

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

### Docker

A two-service stack ships in the repo: the gateway plus a built-in mock
upstream so you can `up` and see a working chat response without configuring
any real provider.

```bash
docker compose up --build
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{"model":"chat","messages":[{"role":"user","content":"hello"}]}'
```

To point the containerised gateway at a real upstream, edit
`configs/config.docker.yaml` (it is bind-mounted into the container) and
restart with `docker compose up -d --force-recreate`. The same config keys
apply as in the non-Docker Quick Start above.

## Authentication

Authentication supports three modes configured via `auth.mode`:

- `none`: no authentication. Local use only.
- `static`: a single Bearer token from a configured environment variable.
- `api-key`: per-key Bearer authentication, with per-key rate limits and daily token quotas. Keys are organized under one or more teams.

### Static token

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

### API keys with team grouping

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
      - id: key-batch
        key: sk-replace-me-too
        limits:
          rps: 5
          burst: 10
          preday_tokens: 1000000
```

```bash
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer sk-replace-me-with-a-generated-key"
```

Generate a fresh key with the bundled helper:

```bash
go run ./cmd/keygen
./scripts/genkey.sh
```

The command only prints a key. Paste it into `configs/config.yaml` under the appropriate `api_keys[].key`. The YAML file is the single source of truth.

Per-key limits:

- `rps` (float): sustained token-bucket rate. `0` means unlimited.
- `burst` (int): bucket capacity. Defaults to `ceil(rps)` when omitted.
- `preday_tokens` (int): daily token quota reset at UTC midnight. Charges on successful chat completions and embeddings. `0` means unlimited.

Responses carry the following headers when the relevant limit is configured:

- `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `Retry-After`
- `X-Quota-Limit-Tokens`, `X-Quota-Used-Tokens`, `X-Quota-Remaining-Tokens`, `X-Quota-Reset-At`

Limits are enforced per process. In multi-replica deployments each replica enforces its own budget; cross-replica coordination is intentionally out of scope for the in-memory default.

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

## Provider Adapters

The public API remains OpenAI-compatible. `provider.type: openai` uses OpenAI-compatible upstream endpoints; `provider.type: anthropic` calls the Anthropic Messages API with `x-api-key` authentication and the `anthropic-version: 2023-06-01` header.

For Anthropic providers, Chat Completions supports system and text messages, function tools, tool results, `tool_choice`, non-streaming responses, SSE, and token usage. Embeddings are routed only to OpenAI-compatible providers. Images, audio, documents/files, non-function tools, structured response formats, logprobs, penalties, seed, non-default `n`, user attribution, and reasoning-specific fields return a local OpenAI-style `400` rather than being sent upstream.

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
