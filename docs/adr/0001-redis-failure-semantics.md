# ADR 0001: Redis failure semantics

Date: 2026-08-23
Status: accepted

## Context

The gateway keeps distributed state (rate-limit buckets, quota counters,
guardrail penalties, revoked keys) in Redis. A production question: what
should happen when Redis becomes unreachable, and what survives a Redis
restart?

## Decision

1. **Every Redis-backed layer fails open.** A transient Redis outage
   degrades limits and revocation checks but never turns the gateway into
   a hard 503 wall. Rationale: the gateway is a traffic front door;
   denying all traffic because a supporting store is down amplifies a
   small failure into a full outage. The trade-off is accepted and
   documented (alert on error spikes).
2. **Rate-limit/quota/guardrail counters are ephemeral by design**
   (TTL-bounded). A Redis restart resets them; operators accept the reset
   semantics for quota windows.
3. **Revocations default to permanent and MUST be backed by persistence.**
   `admin.revoke_ttl` defaults to 0 (permanent). Because revocations are
   a security control, deployments that use them should enable AOF
   (`appendonly yes`) so a restart does not silently un-revoke a leaked
   key. The runbook and deploy manifests carry commented toggles.
4. **`readyz` is fail-closed.** A replica whose Redis probe fails reports
   503 so orchestrators stop routing traffic to it — this is the
   deliberate exception to fail-open, because it removes the replica
   from the pool rather than serving degraded.
5. **Sentinel/Cluster failover is supported at the connection layer**
   (redisutil.UniversalClient) so HA is a configuration change, not a
   code change.

## Consequences

- Operators must alert on `redis` error logs and the `readyz` 503s rather
  than relying on enforcement to hold during an outage.
- A Redis restart resets quota windows; usage reporting (SQLite sink)
  remains the source of truth for billing.
- Revocation durability depends on deployment persistence; documented in
  docs/operations.md.