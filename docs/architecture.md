# Architecture

```text
client -> concurrency -> auth -> [before_request plugins] -> alias resolver -> adapter filter -> strategy order -> retry/failover -> provider adapter -> upstream
                                        \-> usage sink (best effort after result)
```

The public HTTP gateway is OpenAI-compatible. Routing translates a client-visible alias into an ordered provider list; adapter capability filtering removes providers that cannot satisfy the requested operation before retry/failover begins. The alias's routing strategy then reorders the chain per request: `fallback` keeps priority order, `loadbalance` picks a weighted-random primary, and `least_latency` tries the fastest observed provider first (EWMA fed by every routed attempt). `openai` providers retain raw JSON forwarding with only the resolved-model rewrite for Chat Completions, Responses, and Embeddings; `anthropic` providers translate the supported Chat Completions subset to the Messages API and map responses back to OpenAI envelopes. Responses API passthrough is currently available only for `openai` providers.

## Middleware Pipeline

Requests pass through the following middleware chain:

1. **concurrency** — global in-flight semaphore (503 when saturated)
2. **auth** — Bearer token or API key validation, injects principal into context
3. **rate limit / quota** — token-bucket rate limiter and daily/monthly quotas (enforced in auth)

After a request body is parsed, staged plugins run inside the handler:

1. **before_request** — runs before quota checks and the provider call; may
   reject the request (a guardrail tripping → 4xx/429) or rewrite the body.
   A broken plugin fails closed except observability types, which fail open.
2. **after_request** — runs before a non-streaming response is written; may
   screen or rewrite it.
3. **on_error** — runs after a failed request with the error in context; it
   never changes the primary error response.

Guardrails are the first plugin (before_request).

## Retry & Failover

Retry executes eligible failures on the current provider and then moves to a lower-priority provider. Streaming responses never fail over after their first emitted client-visible event.

## Connection Pool Isolation

Each provider type (`openai`, `anthropic`) maintains its own `*http.Client` with an isolated `*http.Transport`. This prevents one provider's connection issues from affecting others.

## Guardrails (Prompt Injection Protection)

Lightweight, zero-external-dependency security layer that runs as a
`before_request` plugin (previously the HTTP middleware):

- **Regex Scanner** — 90+ patterns across 15 categories (EN/ZH/JA/RU), composite scoring
- **Canary Tokens** — random markers in system messages detect prompt leakage with zero false positives
- **Per-Key Tracker** — automatic rate limiting for repeat offenders

Operates in `flag` (log only) or `block` (reject) mode. Default: `off`. In
`block` mode a flagged request is refused with a `prompt_injection_detected`
rejection; a tracker-blocked key gets a `injection_tracker_blocked` rejection
with a `Retry-After` header.

## Distributed State

Rate limits, token quotas, and guardrail tracking default to process-local memory. In a multi-replica deployment, configure the Redis drivers for the shared state you need; the Docker example provides the expected `rate_limit`, `quota`, and Redis address settings. Redis-backed hot paths fail open when the backend is temporarily unavailable, so availability is preserved at the cost of temporarily relaxed enforcement.
