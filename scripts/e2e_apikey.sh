#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
primary_pid=""
gateway_pid=""
failed=1
cleanup() {
  local exit_code=$?
  for pid in "$gateway_pid" "$primary_pid"; do
    [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && { kill -TERM "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
  done
  if [[ $failed -ne 0 ]]; then
    for log in "$tmp_dir"/*.log; do
      [[ -f "$log" ]] || continue
      printf '\n--- %s ---\n' "$(basename "$log")" >&2
      cat "$log" >&2
    done
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

pick_port() { python3 - <<'PY'
import socket
sock=socket.socket(); sock.bind(("127.0.0.1",0)); print(sock.getsockname()[1]); sock.close()
PY
}

primary_port="$(pick_port)"; gateway_port="$(pick_port)"; health_port="$(pick_port)"
primary_url="http://127.0.0.1:${primary_port}"
gateway_url="http://127.0.0.1:${gateway_port}"

cd "$root_dir"
go build -o "$tmp_dir/mock-upstream" ./scripts/mock_upstream.go
go build -o "$tmp_dir/gateway" ./cmd/gateway

"$tmp_dir/mock-upstream" -listen "127.0.0.1:${primary_port}" -role primary >"$tmp_dir/primary.log" 2>&1 &
primary_pid=$!
for _ in $(seq 1 100); do curl --fail --silent "${primary_url}/healthz" >/dev/null && break; sleep 0.05; done

API_KEY="sk-$(go run ./cmd/keygen)"
API_KEY2="sk-$(go run ./cmd/keygen)"

cat >"$tmp_dir/config.yaml" <<EOF
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
    providers:
      - {name: primary, model: primary-chat, priority: 0}
  embedding:
    providers:
      - {name: primary, model: primary-embedding, priority: 0}
teams:
  - id: team-demo
    api_keys:
      - id: key-prod
        key: ${API_KEY}
        limits:
          rps: 100
          burst: 100
          preday_tokens: 100000
      - id: key-tight
        key: ${API_KEY2}
        limits:
          rps: 1
          burst: 1
          preday_tokens: 5
retry:
  enabled: true
  max_attempts_per_provider: 1
failover:
  enabled: false
EOF

PRIMARY_UPSTREAM_TOKEN=primary-token "$tmp_dir/gateway" --config "$tmp_dir/config.yaml" >"$tmp_dir/gateway.log" 2>&1 &
gateway_pid=$!
for _ in $(seq 1 100); do curl --fail --silent "http://127.0.0.1:${health_port}/healthz" >/dev/null && break; sleep 0.05; done

set -x
echo "--- invalid key rejected ---"
status="$(curl --silent --output "$tmp_dir/unauth.json" --write-out '%{http_code}' "${gateway_url}/v1/models" -H 'Authorization: Bearer wrong')"
[[ "$status" == "401" ]] || { echo "expected 401, got $status"; cat "$tmp_dir/unauth.json"; exit 1; }

echo "--- valid key passes and headers present ---"
status="$(curl --silent -D "$tmp_dir/ok.headers" --output "$tmp_dir/ok.json" --write-out '%{http_code}' "${gateway_url}/v1/models" -H "Authorization: Bearer ${API_KEY}")"
[[ "$status" == "200" ]] || { echo "expected 200, got $status"; cat "$tmp_dir/ok.json"; exit 1; }
grep -qi '^X-Quota-Limit-Tokens' "$tmp_dir/ok.headers"

echo "--- chat with usage hits quota headers ---"
chat='{"model":"chat","messages":[{"role":"user","content":"hello"}],"metadata":{"e2e_case":"api_key_chat"}}'
status="$(curl --silent -D "$tmp_dir/chat.headers" --output "$tmp_dir/chat.json" --write-out '%{http_code}' "${gateway_url}/v1/chat/completions" -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' --data "$chat")"
[[ "$status" == "200" ]] || { echo "expected 200, got $status"; cat "$tmp_dir/chat.json"; exit 1; }
grep -qi '^X-Quota-Used-Tokens' "$tmp_dir/chat.headers"

echo "--- tight rps key is rejected with 429 after burst ---"
status1="$(curl --silent -o "$tmp_dir/tight1.json" --write-out '%{http_code}' "${gateway_url}/v1/models" -H "Authorization: Bearer ${API_KEY2}")"
status2="$(curl --silent -D "$tmp_dir/tight2.headers" -o "$tmp_dir/tight2.json" --write-out '%{http_code}' "${gateway_url}/v1/models" -H "Authorization: Bearer ${API_KEY2}")"
[[ "$status1" == "200" ]] || { echo "tight1 expected 200 got $status1"; exit 1; }
[[ "$status2" == "429" ]] || { echo "tight2 expected 429 got $status2"; cat "$tmp_dir/tight2.json"; exit 1; }
grep -qi '^Retry-After' "$tmp_dir/tight2.headers"

echo "--- tiny quota key exhausts after one chat ---"
chat2='{"model":"chat","messages":[{"role":"user","content":"hello"}],"metadata":{"e2e_case":"api_key_quota"}}'
for _ in 1 2 3 4 5 6 7 8 9 10; do
  status="$(curl --silent -o /dev/null --write-out '%{http_code}' "${gateway_url}/v1/chat/completions" -H "Authorization: Bearer ${API_KEY2}" -H 'Content-Type: application/json' --data "$chat2")"
  echo "  status=$status"
  if [[ "$status" == "429" ]]; then break; fi
  sleep 1
done
[[ "$status" == "429" ]] || { echo "expected 429 for quota exceeded, got $status"; exit 1; }

failed=0
echo "API-key e2e passed."