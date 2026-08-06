#!/usr/bin/env bash
set -euo pipefail

base_url="${GATEWAY_URL:-http://127.0.0.1:8080}"

curl --fail --silent "${base_url}/v1/models" | grep -q '"chat"'
curl --fail --silent "${base_url}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  --data '{"model":"chat","messages":[{"role":"user","content":"ping"}]}' | grep -q 'mock response'
curl --fail --silent "${base_url}/v1/embeddings" \
  -H 'Content-Type: application/json' \
  --data '{"model":"embedding","input":"ping"}' | grep -q '"embedding"'
