#!/usr/bin/env bash
set -euo pipefail

HOST="${1:-127.0.0.1:50051}"
echo "Target: $HOST"

ghz --insecure \
  --proto proto/ratelimit.proto \
  --call ratelimit.v1.RateLimitService.Allow \
  -d '{"key":"user123","rule":"1000rps","algorithm":"TOKEN_BUCKET","namespace":"api","cost":1}' \
  -c 200 -n 200000 \
  "$HOST"

ghz --insecure \
  --proto proto/ratelimit.proto \
  --call ratelimit.v1.RateLimitService.Allow \
  -d '{"key":"user123","rule":"1000/1s","algorithm":"SLIDING_WINDOW","namespace":"api","cost":1}' \
  -c 200 -n 200000 \
  "$HOST"