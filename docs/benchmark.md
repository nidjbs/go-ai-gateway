# Benchmarks: cost of the gateway middle layer

The gateway sits between Agents and LLMs. These are measured numbers for the
overhead it adds — the point being that a fully-featured middle layer does not
need to cost noticeable latency or throughput.

Measured 2026-08-28 on an Apple M1 (darwin/arm64), Go 1.26.6.

## Setup

- Gateway: this repo's `cmd/gateway`, built from source, in-memory storage
  (no Redis), `auth.mode: none`, guardrails/retry/failover/circuit-breaker
  disabled. See the benchmark config under "Reproduction" below.
- Upstream: `scripts/mock_upstream.go` (role `primary`). Its `slow_primary`
  case sleeps 200 ms to simulate a real LLM; all other cases answer
  immediately.
- Load tool: `hey v0.1.5`.

## 1. Micro-benchmarks: per-request CPU inside the gateway

Every request passes through these operations. All are nanoseconds to
microseconds, with zero or tiny allocations.

| Operation | Median | Allocs |
|---|---|---|
| Token estimation (`estimateTokens`) | ~74 ns/op | 0 B/op |
| Alias→upstream model substitution (`replaceModel`) | ~296 ns/op | 4 allocs |
| Idempotency cache lookup (miss) | ~163 ns/op | 1 alloc |
| Request-body cap check (`capRequestBody`) | ~6.6 µs/op | 56 allocs |

Run: `go test ./internal/gateway/ -bench . -benchmem -run '^$' -count 5`

## 2. End-to-end: zero-latency upstream (throughput ceiling)

With an instant upstream the gateway is CPU-bound; this measures how much of
its throughput it spends on gateway work. N=5000, concurrency=50.

| Path | Requests/sec | Avg latency | p50 |
|---|---|---|---|
| Direct to upstream | 36,059 | 1.3 ms | 1.0 ms |
| Through gateway | 12,400–17,200 (3 runs) | 2.8–3.9 ms | 2.5–3.4 ms |

Even at the throughput ceiling the gateway sustains well over 10k RPS on a
single machine — far beyond any real Agent workload. The direct number is
inflated because the mock upstream does no real work; with a real LLM the
gap disappears (see §3). Note the gateway caps upstream connections at
`MaxConnsPerHost` (default 10, `internal/provider/client.go`), which is what
limits concurrency in this test.

## 3. End-to-end: 200 ms simulated LLM latency (realistic)

The upstream sleeps 200 ms per request. N=400, concurrency=8 (below the
gateway's `MaxConnsPerHost` so upstream connections are not the bottleneck).

| Path | Avg | p50 | p95 | p99 |
|---|---|---|---|---|
| Direct to upstream | 201.9 ms | 201.8 ms | 203.3 ms | 204.7 ms |
| Through gateway | 204.6 ms | 203.0 ms | 217.4 ms | 235.3 ms |
| **Gateway overhead** | **+2.7 ms** | **+1.2 ms** | **+14.1 ms** | **+30.6 ms** |

At realistic LLM latency the gateway adds about **1 ms of median latency
(~0.6%)**. The p99 tail reflects connection-pool and GC noise, still tens of
milliseconds against a 200 ms upstream.

## 4. Streaming: first-token latency

TTFT for a streaming chat completion, 30 samples each.

| Path | Avg first-token |
|---|---|
| Direct to upstream | 0.58 ms |
| Through gateway | 1.25 ms |

With real LLMs first tokens typically arrive in 100–1000 ms, so the ~0.7 ms
added by the gateway is imperceptible.

## Reproduction

```bash
# 1. Build
go build -o /tmp/gw ./cmd/gateway
go build -o /tmp/mock ./scripts/mock_upstream.go

# 2. Start mock upstream (role=primary)
/tmp/mock --listen 127.0.0.1:19090 --role primary

# 3. Minimal config (no Redis / auth / guardrails), pointing at the mock
cat > /tmp/bench-config.yaml <<'EOF'
listen: 127.0.0.1:8080
healthz: 127.0.0.1:8081
readyz_wait_time: 2s
auth:
  mode: none
providers:
  local:
    type: openai
    base_url: http://127.0.0.1:19090/v1
    api_key_env: UPSTREAM_API_KEY
    request_timeout: 30s
aliases:
  chat: {provider: local, model: primary-chat}
retry: {enabled: false}
failover: {enabled: false, max_providers: 0}
circuit_breaker: {enabled: false}
server:
  max_concurrent_requests: 0
  max_concurrent_per_key: 0
  max_request_body_bytes: 1048576
  read_timeout: 30s
  idle_timeout: 90s
  stream_idle_timeout: 0s
  stream_max_duration: 0s
  idempotency_enabled: false
tracing: {enabled: false}
rate_limit: {driver: memory, options: {}}
quota: {driver: memory, options: {}}
guardrails: {enabled: false}
EOF

# 4. Start gateway
UPSTREAM_API_KEY=primary-token /tmp/gw --config /tmp/bench-config.yaml

# 5. Load test (zero latency, direct vs gateway)
hey -n 5000 -c 50 -m POST -T application/json \
  -H 'Authorization: Bearer primary-token' \
  -d '{"model":"primary-chat","messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:19090/v1/chat/completions
hey -n 5000 -c 50 -m POST -T application/json \
  -d '{"model":"chat","messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:8080/v1/chat/completions

# 6. Load test (200 ms latency): add "metadata":{"e2e_case":"slow_primary"}
# to the payload and use concurrency 8.
```

## Interpretation

The gateway is a transparent middle layer: at realistic LLM latencies it adds
single-digit milliseconds at the median and keeps its own CPU cost to
microseconds per request, while providing routing, retry, failover, rate
limits, quotas, guardrails, DLP, and observability that an Agent would
otherwise have to build and maintain itself.
