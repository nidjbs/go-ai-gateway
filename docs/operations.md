# Operations Guide

Production-run guidance for a multi-replica go-ai-gateway deployment.

## Topology

```text
            ┌───────────────┐      ┌──────────────────────┐
 clients ──►│ LB / Ingress  ├────► │ gateway replica 1..N │
            └───────────────┘      └──────────┬───────────┘
                                              │
                              ┌───────────────┼───────────────┐
                              ▼               ▼               ▼
                         Redis (shared)   upstream(s)   ops port (8081)
```

All replicas are stateless: rate limits, quotas, and guardrail tracking live
in Redis. With no Redis configured they degrade to per-process memory, which
is **not** safe for multi-replica deployments (limits are enforced per
replica, not globally).

## Probes

| Probe | Port | Semantics |
|---|---|---|
| `/healthz` / `/livez` | ops | always 200 while the process is up |
| `/readyz` | ops | 204 only after the startup window **and** when every configured dependency probe passes (Redis when a `redis` driver is configured) |
| `/version` | ops | build metadata (version/commit/build_date), injected via `-ldflags` |
| `/metrics` | ops | Prometheus exposition |

`/readyz` is dependency-aware: a replica whose Redis is unreachable returns
503, so orchestrators stop sending it traffic instead of serving fail-open.

## Scaling

- Scale replicas behind a load balancer; a shared Redis makes limits global.
- HPA: see `deploy/k8s.yaml` (CPU-based). In-flight concurrency is already
  capped server-side via `server.max_concurrent_requests` (503 +
  `server_overloaded` when saturated), so a saturated replica drains cleanly.
- Graceful shutdown is built in: SIGTERM sets `draining` (readyz → 503),
  stops accepting new requests, drains in-flight work, then exits.

## Circuit breaker

`circuit_breaker.enabled: true` is recommended once you have at least one
failover candidate. With a single provider, an open breaker makes requests
fail fast (502) instead of hammering a dead upstream — pair it with alerting
on `upstream_error` / `upstream_unavailable` error types in metrics.

## Secrets and key rotation

- Upstream API keys: `api_key_env` reads from the environment/Secret —
  never put real keys in the YAML.
- Client API keys (`auth.mode: api-key`): configure **hashes** instead of
  plaintext. Generate with:

  ```bash
  go run ./cmd/keygen -sha256   # prints sha256:<hex>
  ```

  and put the digest in `teams[].api_keys[].key`. The plaintext is shown to
  the operator exactly once, at generation time. Rotation = add the new
  digest, restart, remove the old digest.
- Ops endpoints: set `ops_token_env` to require a bearer token on
  `/metrics`, `/readyz`, `/version`.

## Quota & cost controls

Per key:
- `preday_tokens` / `monthly_tokens` — token quotas (UTC windows).
- `max_requests_per_day` — hard request counter (charged before execution).
- `max_tokens_per_request` — ceiling on `max_tokens` /
  `max_completion_tokens` / `max_output_tokens` in the request body.
- `alias_preday_tokens` — per-model daily quotas.

Token quotas are charged from upstream-reported usage. When an upstream omits
usage fields, or a stream ends without a usage chunk (failure, timeout, or
client abort), the gateway charges an **estimate** derived from request size
and emitted bytes, so quota/cost enforcement does not silently disappear.

## Idempotency (avoid double-charging retries)

Set `server.idempotency_enabled: true`. Clients that send an
`Idempotency-Key` header on non-streaming `/v1/chat/completions` /
`/v1/responses` requests receive the cached response on retry instead of a
second upstream execution and second charge. The cache is per (api key, key),
TTL-bounded (`server.idempotency_ttl`, default 1h), and capped at 4096
entries.

## Stream timeouts

- `server.stream_idle_timeout` — kill streams whose upstream goes silent
  (gap between events), writing an SSE error frame.
- `server.stream_max_duration` — hard ceiling on total stream lifetime.

Both are off by default (`0`). Set them in production to bound resources.

## Guardrails in block mode

`guardrails.mode: block` rejects requests that trip the injection scanner.
False-positive-prone payloads (structured tool data, benchmark suites) can be
exempted per-substring via `guardrails.allowlist` without disabling the
layer globally.

## Load testing

```bash
scripts/load_test.sh http://gateway/v1/chat/completions sk-token   # hey or curl fan-out
go test ./internal/gateway/ -bench=. -benchmem
```

## Alerts worth setting up

- `ai_gateway_llm_request_duration` p95 climbing.
- error.type `upstream_error`, `upstream_timeout`, `upstream_unavailable`.
- `quota_exceeded_*` bursts (key compromised or limits too low).
- `injection_tracker_blocked` (guardrails enforcement firing).

