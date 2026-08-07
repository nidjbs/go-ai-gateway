#!/usr/bin/env bash
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

assert_json() {
  local file=$1
  local program=$2
  python3 - "$file" "$program" <<'PY'
import json
import sys

with open(sys.argv[1]) as handle:
    value = json.load(handle)
namespace = {"value": value, "assert_true": lambda condition, message: (_ for _ in ()).throw(AssertionError(message)) if not condition else None}
exec(sys.argv[2], namespace)
PY
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

run_case() {
  printf '[RUN ] %s\n' "$1"
}

pass_case() {
  printf '[PASS] %s\n' "$1"
}

primary_port="$(pick_port)"
backup_port="$(pick_port)"
gateway_port="$(pick_port)"
health_port="$(pick_port)"
primary_url="http://127.0.0.1:${primary_port}"
backup_url="http://127.0.0.1:${backup_port}"
gateway_url="http://127.0.0.1:${gateway_port}"

cd "$root_dir"
go build -o "$tmp_dir/mock-upstream" ./scripts/mock_upstream.go
go build -o "$tmp_dir/gateway" ./cmd/gateway
"$tmp_dir/mock-upstream" -listen "127.0.0.1:${primary_port}" -role primary >"$tmp_dir/primary.log" 2>&1 &
primary_pid=$!
"$tmp_dir/mock-upstream" -listen "127.0.0.1:${backup_port}" -role backup >"$tmp_dir/backup.log" 2>&1 &
backup_pid=$!
wait_for "${primary_url}/healthz" primary
wait_for "${backup_url}/healthz" backup

cat >"$tmp_dir/config.yaml" <<EOF
listen: 127.0.0.1:${gateway_port}
healthz: 127.0.0.1:${health_port}
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
  chat:
    providers:
      - {name: backup, model: backup-chat, priority: 1}
      - {name: primary, model: primary-chat, priority: 0}
  embedding:
    providers:
      - {name: backup, model: backup-embedding, priority: 1}
      - {name: primary, model: primary-embedding, priority: 0}
retry:
  enabled: true
  max_attempts_per_provider: 2
  max_elapsed_time: 5s
  initial_interval: 1ms
  max_interval: 1ms
  multiplier: 1
  jitter: 0
  retryable_statuses: [503]
failover:
  enabled: true
EOF

GATEWAY_STATIC_TOKEN=gateway-token PRIMARY_UPSTREAM_TOKEN=primary-token BACKUP_UPSTREAM_TOKEN=backup-token "$tmp_dir/gateway" --config "$tmp_dir/config.yaml" >"$tmp_dir/gateway.log" 2>&1 &
gateway_pid=$!
wait_for "http://127.0.0.1:${health_port}/healthz" gateway

run_case "health, auth, and model aliases"
assert_status 200 "$(request GET /healthz '' '' "$tmp_dir/health.json")" "$tmp_dir/health.json"
assert_status 401 "$(request GET /v1/models '' '' "$tmp_dir/models-unauthorized.json")" "$tmp_dir/models-unauthorized.json"
assert_status 401 "$(request GET /v1/models '' wrong-token "$tmp_dir/models-wrong-token.json")" "$tmp_dir/models-wrong-token.json"
assert_status 200 "$(request GET /v1/models '' gateway-token "$tmp_dir/models.json")" "$tmp_dir/models.json"
assert_json "$tmp_dir/models.json" 'ids = {item["id"] for item in value["data"]}; assert_true(ids == {"chat", "embedding"}, ids)'
pass_case "health, auth, and model aliases"

run_case "agent tool-call loop"
tool_one='{"model":"chat","messages":[{"role":"user","content":"What is the weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":"auto","metadata":{"e2e_case":"tool_round_1"}}'
assert_status 200 "$(request POST /v1/chat/completions "$tool_one" gateway-token "$tmp_dir/tool-one.json")" "$tmp_dir/tool-one.json"
assert_json "$tmp_dir/tool-one.json" 'choice = value["choices"][0]; call = choice["message"]["tool_calls"][0]; assert_true(value["model"] == "chat", value["model"]); assert_true(choice["finish_reason"] == "tool_calls", choice); assert_true(call["id"] == "call_weather_1" and call["function"]["name"] == "get_weather", call)'
tool_two='{"model":"chat","messages":[{"role":"user","content":"What is the weather?"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_weather_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\",\"units\":\"metric\"}"}}]},{"role":"tool","tool_call_id":"call_weather_1","name":"get_weather","content":"{\"temperature\":26}"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"metadata":{"e2e_case":"tool_round_2"}}'
assert_status 200 "$(request POST /v1/chat/completions "$tool_two" gateway-token "$tmp_dir/tool-two.json")" "$tmp_dir/tool-two.json"
assert_json "$tmp_dir/tool-two.json" 'assert_true(value["model"] == "chat", value); assert_true(value["choices"][0]["message"]["content"] == "tool result accepted", value)'
[[ "$(stats_count "$primary_url" tool_round_1)" == 1 && "$(stats_count "$primary_url" tool_round_2)" == 1 ]]
pass_case "agent tool-call loop"

run_case "multi-turn chat"
multi_turn='{"model":"chat","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hello back"},{"role":"user","content":"continue"}],"metadata":{"e2e_case":"multi_turn"}}'
assert_status 200 "$(request POST /v1/chat/completions "$multi_turn" gateway-token "$tmp_dir/multi-turn.json")" "$tmp_dir/multi-turn.json"
assert_json "$tmp_dir/multi-turn.json" 'assert_true(value["choices"][0]["message"]["content"] == "multi-turn accepted", value)'
pass_case "multi-turn chat"

run_case "successful SSE stream"
stream_success='{"model":"chat","stream":true,"messages":[{"role":"user","content":"stream"}],"metadata":{"e2e_case":"stream_success"}}'
status="$(request POST /v1/chat/completions "$stream_success" gateway-token "$tmp_dir/stream-success.sse")"
assert_status 200 "$status" "$tmp_dir/stream-success.sse"
grep -qi '^Content-Type: text/event-stream' "$tmp_dir/stream-success.sse.headers"
python3 - "$tmp_dir/stream-success.sse" <<'PY'
import json
import sys
lines = [line[6:] for line in open(sys.argv[1]) if line.startswith("data: ")]
assert lines[-1].strip() == "[DONE]", lines
chunks = [json.loads(line) for line in lines[:-1]]
assert len(chunks) == 1 and chunks[0]["model"] == "chat", chunks
assert chunks[0]["choices"][0]["delta"]["content"] == "stream success", chunks
PY
pass_case "successful SSE stream"

run_case "SSE failover before data"
stream_failover='{"model":"chat","stream":true,"messages":[{"role":"user","content":"stream"}],"metadata":{"e2e_case":"stream_failover"}}'
assert_status 200 "$(request POST /v1/chat/completions "$stream_failover" gateway-token "$tmp_dir/stream-failover.sse")" "$tmp_dir/stream-failover.sse"
grep -q 'backup stream' "$tmp_dir/stream-failover.sse"
grep -q 'data: \[DONE\]' "$tmp_dir/stream-failover.sse"
[[ "$(stats_count "$primary_url" stream_failover)" == 2 && "$(stats_count "$backup_url" stream_failover)" == 1 ]]
pass_case "SSE failover before data"

run_case "SSE abort after first chunk does not fail over"
stream_abort='{"model":"chat","stream":true,"messages":[{"role":"user","content":"stream"}],"metadata":{"e2e_case":"stream_abort_after_chunk"}}'
assert_status 200 "$(request POST /v1/chat/completions "$stream_abort" gateway-token "$tmp_dir/stream-abort.sse")" "$tmp_dir/stream-abort.sse"
grep -q 'primary partial' "$tmp_dir/stream-abort.sse"
grep -q 'upstream request failed' "$tmp_dir/stream-abort.sse"
! grep -q 'data: \[DONE\]' "$tmp_dir/stream-abort.sse"
[[ "$(stats_count "$primary_url" stream_abort_after_chunk)" == 1 && "$(stats_count "$backup_url" stream_abort_after_chunk)" == 0 ]]
pass_case "SSE abort after first chunk does not fail over"

run_case "non-stream retry and failover"
chat_retry='{"model":"chat","messages":[{"role":"user","content":"retry"}],"metadata":{"e2e_case":"chat_retry"}}'
assert_status 200 "$(request POST /v1/chat/completions "$chat_retry" gateway-token "$tmp_dir/chat-retry.json")" "$tmp_dir/chat-retry.json"
grep -q 'primary retry success' "$tmp_dir/chat-retry.json"
[[ "$(stats_count "$primary_url" chat_retry)" == 2 && "$(stats_count "$backup_url" chat_retry)" == 0 ]]
chat_failover='{"model":"chat","messages":[{"role":"user","content":"failover"}],"metadata":{"e2e_case":"chat_failover"}}'
assert_status 200 "$(request POST /v1/chat/completions "$chat_failover" gateway-token "$tmp_dir/chat-failover.json")" "$tmp_dir/chat-failover.json"
grep -q 'backup failover success' "$tmp_dir/chat-failover.json"
[[ "$(stats_count "$primary_url" chat_failover)" == 2 && "$(stats_count "$backup_url" chat_failover)" == 1 ]]
pass_case "non-stream retry and failover"

run_case "embeddings and validation"
embedding_one='{"model":"embedding","input":"hello","metadata":{"e2e_case":"embedding_one"}}'
assert_status 200 "$(request POST /v1/embeddings "$embedding_one" gateway-token "$tmp_dir/embedding-one.json")" "$tmp_dir/embedding-one.json"
assert_json "$tmp_dir/embedding-one.json" 'assert_true(value["model"] == "embedding" and len(value["data"]) == 1, value)'
embedding_many='{"model":"embedding","input":["one","two"],"metadata":{"e2e_case":"embedding_many"}}'
assert_status 200 "$(request POST /v1/embeddings "$embedding_many" gateway-token "$tmp_dir/embedding-many.json")" "$tmp_dir/embedding-many.json"
assert_json "$tmp_dir/embedding-many.json" 'assert_true(value["model"] == "embedding" and len(value["data"]) == 2, value)'
embedding_failover='{"model":"embedding","input":["one","two"],"metadata":{"e2e_case":"embedding_failover"}}'
assert_status 200 "$(request POST /v1/embeddings "$embedding_failover" gateway-token "$tmp_dir/embedding-failover.json")" "$tmp_dir/embedding-failover.json"
[[ "$(stats_count "$primary_url" embedding_failover)" == 2 && "$(stats_count "$backup_url" embedding_failover)" == 1 ]]
unknown='{"model":"missing","messages":[{"role":"user","content":"hello"}]}'
assert_status 400 "$(request POST /v1/chat/completions "$unknown" gateway-token "$tmp_dir/unknown.json")" "$tmp_dir/unknown.json"
empty_embedding='{"model":"embedding","input":""}'
assert_status 400 "$(request POST /v1/embeddings "$empty_embedding" gateway-token "$tmp_dir/empty-embedding.json")" "$tmp_dir/empty-embedding.json"
pass_case "embeddings and validation"

failed=0
printf 'All E2E cases passed.\n'
