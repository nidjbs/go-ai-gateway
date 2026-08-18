# Architecture

```text
client -> concurrency -> auth -> guardrails -> alias resolver -> adapter filter -> retry/failover -> provider adapter -> upstream
                                        \-> usage sink (best effort after result)
```

The public HTTP gateway is OpenAI-compatible. Routing translates a client-visible alias into an ordered provider list; adapter capability filtering removes providers that cannot satisfy the requested operation before retry/failover begins. `openai` providers retain raw JSON forwarding with only the resolved-model rewrite, while `anthropic` providers translate the supported Chat Completions subset to the Messages API and map responses back to OpenAI envelopes.

## Middleware Pipeline

Requests pass through the following middleware chain:

1. **concurrency** — global in-flight semaphore (503 when saturated)
2. **auth** — Bearer token or API key validation, injects principal into context
3. **guardrails** — prompt injection detection (regex scan + canary tokens + per-key tracking)
4. **rate limit / quota** — token-bucket rate limiter and daily/monthly quotas (enforced in auth)

## Retry & Failover

Retry executes eligible failures on the current provider and then moves to a lower-priority provider. Streaming responses never fail over after their first emitted client-visible event.

## Connection Pool Isolation

Each provider type (`openai`, `anthropic`) maintains its own `*http.Client` with an isolated `*http.Transport`. This prevents one provider's connection issues from affecting others.

## Guardrails (Prompt Injection Protection)

Lightweight, zero-external-dependency security layer:

- **Regex Scanner** — 90+ patterns across 15 categories (EN/ZH/JA/RU), composite scoring
- **Canary Tokens** — random markers in system messages detect prompt leakage with zero false positives
- **Per-Key Tracker** — automatic rate limiting for repeat offenders

Operates in `flag` (log only) or `block` (reject) mode. Default: `off`.

## Distributed State

Rate limits, token quotas, and guardrail tracking default to process-local memory. In a multi-replica deployment, configure the Redis drivers for the shared state you need; the Docker example provides the expected `rate_limit`, `quota`, and Redis address settings. Redis-backed hot paths fail open when the backend is temporarily unavailable, so availability is preserved at the cost of temporarily relaxed enforcement.
