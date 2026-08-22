#!/usr/bin/env bash
# Quick load smoke test for the gateway.
#
# Usage:
#   scripts/load_test.sh [url] [bearer-token]
#   N=500 C=20 scripts/load_test.sh http://127.0.0.1:8080/v1/chat/completions sk-token
#
# Uses hey when installed (recommended for real load testing); otherwise
# falls back to a curl fan-out and reports a rough requests-per-second figure.
set -euo pipefail

url="${1:-http://127.0.0.1:8080/v1/chat/completions}"
token="${2:-}"
n="${N:-200}"
c="${C:-10}"

payload='{"model":"chat","messages":[{"role":"user","content":"benchmark"}]}'

if command -v hey >/dev/null 2>&1; then
  echo "== hey load test: N=$n concurrency=$c =="
  if [[ -n "$token" ]]; then
    exec hey -n "$n" -c "$c" -m POST -T 'application/json' -H "Authorization: Bearer $token" -d "$payload" "$url"
  else
    exec hey -n "$n" -c "$c" -m POST -T 'application/json' -d "$payload" "$url"
  fi
fi

echo "hey not found; using curl fan-out (N=$n concurrency=$c)"
start="$(date +%s%N)"
for _ in $(seq "$n"); do
  (
    if [[ -n "$token" ]]; then
      code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $token" -d "$payload" "$url")"
    else
      code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$payload" "$url")"
    fi
    [[ "$code" == 2* ]] && echo "$code" > /dev/null
  ) &
  while [[ "$(jobs -r | wc -l)" -ge "$c" ]]; do sleep 0.01; done
done
wait
end="$(date +%s%N)"
elapsed_ms="$(( (end - start) / 1000000 ))"
echo "== completed in ${elapsed_ms}ms =="
if [[ "$elapsed_ms" -gt 0 ]]; then
  echo "approx throughput: $(( n * 1000 / elapsed_ms )) req/s"
fi

