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

## Redis failure semantics & high availability

The gateway's distributed state — rate-limit buckets, quota counters,
guardrail penalty state, and revoked keys — lives in Redis. Every layer
is designed to **fail open**: a transient Redis outage degrades limits
but never turns the gateway into a hard 503 wall. Alert on the error
spikes instead (the layers log their failures).

### Failure matrix

| Layer | Redis down → | Notes |
|---|---|---|
| Rate limits | fail open (requests allowed) | `redisLimiter.Allow` returns allowed on error |
| Quotas | fail open (Peek/Charge return full remaining) | conservative choice: never deny on partial data |
| Guardrail tracker | fail open (no block/penalty) | injection scanning still runs per-request |
| Key revocation | fail open (revoked keys not checked) | revocations resume within the cache TTL after recovery |
| `readyz` | 503 (replica drains from traffic) | orchestrators stop routing to the unhealthy replica |

### What survives a Redis restart

Rate-limit buckets, quota counters, and guardrail counters are **ephemeral
by design** (TTL-bounded). A Redis restart resets them: limits re-fill,
daily/monthly quotas restart, and revocation entries **are lost unless**
the deployment persists Redis (see below). For quota/cost enforcement,
treat Redis as the source of truth and accept the reset semantics — or
pair the gateway's usage sink (SQLite) with alerting on `quota_exceeded_*`
bursts after a Redis restart.

Revocations are the one piece with security implications: keep
`admin.revoke_ttl` unset (permanent) **and** enable Redis persistence
(`appendonly yes`) so a restart does not silently un-revoke a leaked key.

### High availability options

All Redis-backed drivers accept the same connection block, including
Sentinel/Cluster failover. The gateway uses go-redis's UniversalClient,
so switching from a standalone Redis to Sentinel is a config change:

```yaml
rate_limit:
  driver: redis
  options:
    # standalone:
    addr: redis:6379
    password: ""
    # Sentinel (uncomment to switch):
    # sentinel_addrs: sentinel-1:26379,sentinel-2:26379,sentinel-3:26379
    # master_name: mymaster
    # sentinel_password: ""
```

The same keys work under `quota.options`, `guardrails.tracker.driver.options`,
and `admin.revocation.options`. A multi-node Redis Cluster is selected by
listing several addresses in `addr` (comma-separated) without
`master_name`.

### Persistence recommendation

- Default (no persistence): fastest, acceptable for pure rate limiting;
  revocations are **not** durable. Only acceptable with `admin.revoke_ttl`
  or with the knowledge that a restart clears the revoked set.
- `appendonly yes` (AOF): recommended whenever `auth.mode: api-key` is
  used with revocation, or when quota history across restarts matters.
  See the commented toggle in `docker-compose.yaml` and `deploy/k8s.yaml`.
- Sentinel + AOF: the full HA story for multi-replica production.
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

## Admin: key revocation & usage queries

The admin surface lives on the ops port behind its own Bearer token
(`admin.token_env`), separate from the ops token. Enable it only when
needed:

```yaml
admin:
  enabled: true
  token_env: GATEWAY_ADMIN_TOKEN   # required when enabled
  revocation:
    driver: redis                   # shared across replicas
    options: {addr: redis:6379}
```

### Revoke a leaked key (seconds, not a restart)

```bash
curl -X POST http://ops-host:8081/admin/keys/revoke \
  -H 'Authorization: Bearer $GATEWAY_ADMIN_TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{"key_id": "key-prod"}'
curl http://ops-host:8081/admin/keys/revoked \
  -H 'Authorization: Bearer $GATEWAY_ADMIN_TOKEN'
```

Every replica honours the revocation within `admin.cache_ttl` (default
5s). Revocations are permanent unless `admin.revoke_ttl` is set — keep
them permanent and enable Redis AOF so a restart cannot silently
un-revoke a key.

### Cost attribution

With the SQLite usage sink, query who spent what (aggregates over the
recorded window; default 24h):

```bash
curl 'http://ops-host:8081/admin/usage/summary?team_id=frontend&from=2026-08-01T00:00:00Z' \
  -H 'Authorization: Bearer $GATEWAY_ADMIN_TOKEN'
curl 'http://ops-host:8081/admin/usage/series?alias=chat&bucket=day' \
  -H 'Authorization: Bearer $GATEWAY_ADMIN_TOKEN'
```

Filters: `team_id`, `key_id`, `alias`, `from`/`to` (RFC3339). The default
stderr audit sink is not queryable; these endpoints return 501 until a
queryable driver (sqlite) is configured.

### Access log

`server.access_log.enabled: true` emits one structured line per request
with method/path/status/duration/bytes/IP/UA and (when available) the
authenticated key and alias. Bodies are never logged; sampling is
deterministic per request ID (`sample_ratio`). Probe paths are excluded
by default.
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
- `ai_gateway_dlp_detections` (output PII masked/rejected — review whether
  a model change caused a leak).
- `revocation` WARN logs and `revoked_api_key` 401s (key compromise
  response: revoke the key and rotate).

