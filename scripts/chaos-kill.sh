#!/usr/bin/env bash
set -euo pipefail

while true; do
  name=$(docker ps --format '{{.Names}}' | grep -E 'ratelimiter|redis-[1-6]' | shuf -n 1 || true)
  if [ -z "${name}" ]; then
    echo "No matching containers found."
    exit 0
  fi
  echo "CHAOS: stopping $name"
  docker stop "$name" >/dev/null || true
  sleep 2
  echo "CHAOS: starting $name"
  docker start "$name" >/dev/null || true
  sleep 3
done