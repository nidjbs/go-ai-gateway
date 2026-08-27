#!/usr/bin/env bash
# e2e_release.sh — release-gate e2e for the newer gateway capabilities.
#
# Covers: routing strategies (loadbalance / least_latency), output DLP
# (mask + reject), Idempotency-Key replay, the admin surface (revoke /
# usage / revoked list), and the circuit breaker.
#
# Run as part of scripts/release_check.sh before every release. Requires
# Go, Bash, curl, and Python 3.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
primary_pid=""
backup_pid=""
gateway_pid=""
failed=1

cleanup() {
  local exit_code=$?
  for pid in "$gateway_pid" "$primary_pid" "$backup_pid"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [[ $failed -ne 0 ]]; then
    for log_file in "$tmp_dir"/*.log; do
      [[ -f "$log_file" ]] || continue
      printf '\n--- %s ---\n' "$(basename "$log_file")" >&2
      cat "$log_file" >&2
    done
  fi
  if [[ "${KEEP_E2E_LOGS:-0}" == "1" ]]; then
    printf 'E2E logs retained at %s\n' "$tmp_dir"
  else
    rm -rf "$tmp_dir"
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

pick_port() {
  python3 - <<'PY'
import socket
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
}

wait_for() {
  local url=$1
  local name=$2
  for _ in $(seq 1 100); do
    if curl --fail --silent --output /dev/null "$url"; then
      return 0
    fi
    sleep 0.05
  done
  printf '%s did not become ready: %s\n' "$name" "$url" >&2
  return 1
}

assert_status() {
  local expected=$1
  local actual=$2
  local body=$3
  if [[ "$actual" != "$expected" ]]; then
    printf 'status = %s, want %s\n' "$actual" "$expected" >&2
    cat "$body" >&2
    return 1
  fi
}

request() {
  local method=$1
  local path=$2
  local body=$3
  local token=$4
  local response_file=$5
  local headers_file="${response_file}.headers"
  local args=(--silent --show-error --output "$response_file" --dump-header "$headers_file" --request "$method")
  if [[ -n "$token" ]]; then
    args+=(--header "Authorization: Bearer ${token}")
  fi
  if [[ -n "$body" ]]; then
    args+=(--header 'Content-Type: application/json' --data "$body")
  fi
  curl "${args[@]}" "${gateway_url}${path}" || true
  awk 'NR == 1 { print $2 }' "$headers_file"
}

stats_count() {
  local url=$1
  local case_name=$2
  curl --fail --silent "${url}/debug/stats" | python3 -c 'import json, sys; print(json.load(sys.stdin)["by_case"].get(sys.argv[1], 0))' "$case_name"
}

run_case() { printf '[RUN ] %s\n' "$1"; }
pass_case() { printf '[PASS] %s\n' "$1"; }

primary_port="$(pick_port)"
backup_port="$(pick_port)"
primary_url="http://127.0.0.1:${primary_port}"
backup_url="http://127.0.0.1:${backup_port}"
gateway_url=""
health_url=""

cd "$root_dir"
go build -o "$tmp_dir/mock-upstream" ./scripts/mock_upstream.go
go build -o "$tmp_dir/gateway" ./cmd/gateway
"$tmp_dir/mock-upstream" -listen "127.0.0.1:${primary_port}" -role primary >"$tmp_dir/primary.log" 2>&1 &
primary_pid=$!
"$tmp_dir/mock-upstream" -listen "127.0.0.1:${backup_port}" -role backup >"$tmp_dir/backup.log" 2>&1 &
backup_pid=$!
wait_for "${primary_url}/healthz" primary
wait_for "${backup_url}/healthz" backup

# ── Phase A: routing strategies + DLP (mask) + idempotency ────────────────
gateway_port="$(pick_port)"; health_port="$(pick_port)"
gateway_url="http://127.0.0.1:${gateway_port}"
health_url="http://127.0.0.1:${health_port}"
cat >"$tmp_dir/phase-a.yaml" <<EOF
listen: 127.0.0.1:${gateway_port}
healthz: 127.0.0.1:${health_port}
readyz_wait_time: 0s
auth:
  mode: static
  token_env: GATEWAY_STATIC_TOKEN
providers:
  primary:
    type: openai
    base_url: ${primary_url}/v1
    api_key_env: PRIMARY_UPSTREAM_TOKEN
  backup:
    type: openai
    base_url: ${backup_url}/v1
    api_key_env: BACKUP_UPSTREAM_TOKEN
aliases:
  lb:
    strategy: loadbalance
    providers:
      - {name: primary, model: primary-chat, priority: 0, weight: 2}
      - {name: backup, model: backup-chat, priority: 1, weight: 1}
  ll:
    strategy: least_latency
    providers:
      - {name: primary, model: primary-chat, priority: 0}
      - {name: backup, model: backup-chat, priority: 1}
  chat:
    providers:
      - {name: primary, model: primary-chat, priority: 0}
      - {name: backup, model: backup-chat, priority: 1}
retry:
  enabled: true
  max_attempts_per_provider: 1
failover:
  enabled: true
server:
  idempotency_enabled: true
dlp:
  enabled: true
  mode: mask
usage:
  driver: sqlite
  options:
    path: ${tmp_dir}/usage-a.db
EOF
GATEWAY_STATIC_TOKEN=gateway-token PRIMARY_UPSTREAM_TOKEN=primary-token BACKUP_UPSTREAM_TOKEN=backup-token "$tmp_dir/gateway" --config "$tmp_dir/phase-a.yaml" >"$tmp_dir/phase-a.log" 2>&1 &
gateway_pid=$!
wait_for "${health_url}/healthz" "phase-a gateway"

run_case "loadbalance distributes traffic across weighted providers"
for _ in $(seq 1 20); do
  request POST /v1/chat/completions '{"model":"lb","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"lb"}}' gateway-token "$tmp_dir/lb.json" >/dev/null
done
[[ "$(stats_count "$primary_url" lb)" -ge 1 ]] || { echo "loadbalance never hit primary"; exit 1; }
[[ "$(stats_count "$backup_url" lb)" -ge 1 ]] || { echo "loadbalance never hit backup"; exit 1; }
pass_case "loadbalance distributes traffic across weighted providers"

run_case "least_latency shifts traffic to the faster provider"
# First request probes primary (unobserved sorts first, priority order); the
# 200ms primary latency is recorded, then backup is probed and wins the ranking.
for _ in $(seq 1 3); do
  request POST /v1/chat/completions '{"model":"ll","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"slow_primary"}}' gateway-token "$tmp_dir/ll.json" >/dev/null
done
primary_hits="$(stats_count "$primary_url" slow_primary)"
backup_hits="$(stats_count "$backup_url" slow_primary)"
[[ "$primary_hits" -le 1 ]] || { echo "least_latency kept hitting slow primary ($primary_hits times)"; exit 1; }
[[ "$backup_hits" -ge 2 ]] || { echo "least_latency did not shift to backup ($backup_hits times)"; exit 1; }
pass_case "least_latency shifts traffic to the faster provider"

run_case "output DLP masks PII in responses"
assert_status 200 "$(request POST /v1/chat/completions '{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"dlp_output"}}' gateway-token "$tmp_dir/dlp-mask.json")" "$tmp_dir/dlp-mask.json"
! grep -q 'user@example.com' "$tmp_dir/dlp-mask.json" || { echo "email not masked"; exit 1; }
! grep -q '555-123-4567' "$tmp_dir/dlp-mask.json" || { echo "phone not masked"; exit 1; }
grep -q '\[REDACTED\]' "$tmp_dir/dlp-mask.json" || { echo "no mask text in response"; exit 1; }
pass_case "output DLP masks PII in responses"

run_case "Idempotency-Key replays the cached response"
idem_body='{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"idem_replay"}}'
idem_call() {
  local out=$1
  curl --silent --show-error --output "$out" --write-out '%{http_code}' --request POST \
    --header 'Authorization: Bearer gateway-token' \
    --header 'Content-Type: application/json' \
    --header 'Idempotency-Key: release-key-1' \
    --data "$idem_body" "${gateway_url}/v1/chat/completions"
}
assert_status 200 "$(idem_call "$tmp_dir/idem-1.json")" "$tmp_dir/idem-1.json"
assert_status 200 "$(idem_call "$tmp_dir/idem-2.json")" "$tmp_dir/idem-2.json"
cmp -s "$tmp_dir/idem-1.json" "$tmp_dir/idem-2.json" || { echo "replay body differs"; exit 1; }
[[ "$(stats_count "$primary_url" idem_replay)" == 1 ]] || { echo "idempotent replay hit upstream more than once"; exit 1; }
pass_case "Idempotency-Key replays the cached response"

kill -TERM "$gateway_pid" 2>/dev/null || true
wait "$gateway_pid" 2>/dev/null || true
gateway_pid=""

# ── Phase B: DLP (reject) ─────────────────────────────────────────────────
gateway_port="$(pick_port)"; health_port="$(pick_port)"
gateway_url="http://127.0.0.1:${gateway_port}"
health_url="http://127.0.0.1:${health_port}"
cat >"$tmp_dir/phase-b.yaml" <<EOF
listen: 127.0.0.1:${gateway_port}
healthz: 127.0.0.1:${health_port}
readyz_wait_time: 0s
auth:
  mode: static
  token_env: GATEWAY_STATIC_TOKEN
providers:
  primary:
    type: openai
    base_url: ${primary_url}/v1
    api_key_env: PRIMARY_UPSTREAM_TOKEN
aliases:
  chat:
    provider: primary
    model: primary-chat
retry:
  enabled: true
  max_attempts_per_provider: 1
failover:
  enabled: false
dlp:
  enabled: true
  mode: reject
EOF
GATEWAY_STATIC_TOKEN=gateway-token PRIMARY_UPSTREAM_TOKEN=primary-token "$tmp_dir/gateway" --config "$tmp_dir/phase-b.yaml" >"$tmp_dir/phase-b.log" 2>&1 &
gateway_pid=$!
wait_for "${health_url}/healthz" "phase-b gateway"

run_case "output DLP rejects PII in responses"
assert_status 400 "$(request POST /v1/chat/completions '{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"dlp_output"}}' gateway-token "$tmp_dir/dlp-reject.json")" "$tmp_dir/dlp-reject.json"
grep -q 'dlp_rejected' "$tmp_dir/dlp-reject.json" || { echo "no dlp_rejected code in response"; exit 1; }
pass_case "output DLP rejects PII in responses"

kill -TERM "$gateway_pid" 2>/dev/null || true
wait "$gateway_pid" 2>/dev/null || true
gateway_pid=""

# ── Phase C: admin surface (revoke / usage / revoked list) ────────────────
gateway_port="$(pick_port)"; health_port="$(pick_port)"
gateway_url="http://127.0.0.1:${gateway_port}"
health_url="http://127.0.0.1:${health_port}"
API_KEY="$(go run ./cmd/keygen)"
API_KEY2="$(go run ./cmd/keygen)"
cat >"$tmp_dir/phase-c.yaml" <<EOF
listen: 127.0.0.1:${gateway_port}
healthz: 127.0.0.1:${health_port}
readyz_wait_time: 0s
auth:
  mode: api-key
providers:
  primary:
    type: openai
    base_url: ${primary_url}/v1
    api_key_env: PRIMARY_UPSTREAM_TOKEN
aliases:
  chat:
    provider: primary
    model: primary-chat
teams:
  - id: team-release
    api_keys:
      - id: key-revoke-me
        key: ${API_KEY}
        limits: {rps: 100, burst: 100, preday_tokens: 100000}
      - id: key-keep
        key: ${API_KEY2}
        limits: {rps: 100, burst: 100, preday_tokens: 100000}
admin:
  enabled: true
  token_env: ADMIN_TOKEN
  revocation:
    driver: memory
usage:
  driver: sqlite
  options:
    path: ${tmp_dir}/usage-c.db
retry:
  enabled: true
  max_attempts_per_provider: 1
failover:
  enabled: false
EOF
ADMIN_TOKEN=admin-secret PRIMARY_UPSTREAM_TOKEN=primary-token "$tmp_dir/gateway" --config "$tmp_dir/phase-c.yaml" >"$tmp_dir/phase-c.log" 2>&1 &
gateway_pid=$!
wait_for "${health_url}/healthz" "phase-c gateway"

run_case "admin revoke cuts off the key at runtime"
assert_status 200 "$(request POST /v1/chat/completions '{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"admin_before_revoke"}}' "$API_KEY" "$tmp_dir/admin-before.json")" "$tmp_dir/admin-before.json"
revoke_status="$(curl --silent --show-error --output "$tmp_dir/revoke.json" --write-out '%{http_code}' --request POST --header 'Authorization: Bearer admin-secret' --header 'Content-Type: application/json' --data '{"key_id":"key-revoke-me"}' "${health_url}/admin/keys/revoke")"
assert_status 200 "$revoke_status" "$tmp_dir/revoke.json"
assert_status 401 "$(request POST /v1/chat/completions '{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"admin_after_revoke"}}' "$API_KEY" "$tmp_dir/admin-after.json")" "$tmp_dir/admin-after.json"
grep -q 'revoked_api_key' "$tmp_dir/admin-after.json" || { echo "revoked key not refused with revoked_api_key"; exit 1; }
pass_case "admin revoke cuts off the key at runtime"

run_case "admin revoked list and usage query"
assert_status 200 "$(request POST /v1/chat/completions '{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"admin_usage"}}' "$API_KEY2" "$tmp_dir/admin-usage.json")" "$tmp_dir/admin-usage.json"
revoked_status="$(curl --silent --show-error --output "$tmp_dir/revoked.json" --write-out '%{http_code}' --header 'Authorization: Bearer admin-secret' "${health_url}/admin/keys/revoked")"
assert_status 200 "$revoked_status" "$tmp_dir/revoked.json"
grep -q 'key-revoke-me' "$tmp_dir/revoked.json" || { echo "revoked list missing key"; exit 1; }
summary_status="$(curl --silent --show-error --output "$tmp_dir/summary.json" --write-out '%{http_code}' --header 'Authorization: Bearer admin-secret' "${health_url}/admin/usage/summary?key_id=key-keep")"
assert_status 200 "$summary_status" "$tmp_dir/summary.json"
python3 - "$tmp_dir/summary.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
assert s.get("requests", 0) >= 1 and s.get("successes", 0) >= 1, s
PY
pass_case "admin revoked list and usage query"

kill -TERM "$gateway_pid" 2>/dev/null || true
wait "$gateway_pid" 2>/dev/null || true
gateway_pid=""

# ── Phase D: circuit breaker ──────────────────────────────────────────────
gateway_port="$(pick_port)"; health_port="$(pick_port)"
gateway_url="http://127.0.0.1:${gateway_port}"
health_url="http://127.0.0.1:${health_port}"
cat >"$tmp_dir/phase-d.yaml" <<EOF
listen: 127.0.0.1:${gateway_port}
healthz: 127.0.0.1:${health_port}
readyz_wait_time: 0s
auth:
  mode: static
  token_env: GATEWAY_STATIC_TOKEN
providers:
  primary:
    type: openai
    base_url: ${primary_url}/v1
    api_key_env: PRIMARY_UPSTREAM_TOKEN
aliases:
  chat:
    provider: primary
    model: primary-chat
retry:
  enabled: true
  max_attempts_per_provider: 1
failover:
  enabled: false
circuit_breaker:
  enabled: true
  failure_threshold: 3
  open_duration: 30s
  half_open_max_requests: 1
  half_open_success_threshold: 1
EOF
GATEWAY_STATIC_TOKEN=gateway-token PRIMARY_UPSTREAM_TOKEN=primary-token "$tmp_dir/gateway" --config "$tmp_dir/phase-d.yaml" >"$tmp_dir/phase-d.log" 2>&1 &
gateway_pid=$!
wait_for "${health_url}/healthz" "phase-d gateway"

run_case "circuit breaker trips open and stops calling the upstream"
fail_body='{"model":"chat","messages":[{"role":"user","content":"hi"}],"metadata":{"e2e_case":"always_fail"}}'
for i in 1 2 3; do
  assert_status 502 "$(request POST /v1/chat/completions "$fail_body" gateway-token "$tmp_dir/cb-$i.json")" "$tmp_dir/cb-$i.json"
done
[[ "$(stats_count "$primary_url" always_fail)" == 3 ]] || { echo "expected 3 upstream failures before trip"; exit 1; }
for i in 4 5; do
  assert_status 502 "$(request POST /v1/chat/completions "$fail_body" gateway-token "$tmp_dir/cb-$i.json")" "$tmp_dir/cb-$i.json"
done
[[ "$(stats_count "$primary_url" always_fail)" == 3 ]] || { echo "breaker open but upstream was still called"; exit 1; }
grep -q 'upstream_unavailable' "$tmp_dir/cb-5.json" || { echo "open breaker response lacks upstream_unavailable"; exit 1; }
pass_case "circuit breaker trips open and stops calling the upstream"

kill -TERM "$gateway_pid" 2>/dev/null || true
wait "$gateway_pid" 2>/dev/null || true
gateway_pid=""

failed=0
printf 'All release e2e cases passed.\n'
