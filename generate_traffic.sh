#!/bin/bash
while true; do
  curl -s -X POST http://localhost:8080/api/allow \
    -H 'Content-Type: application/json' \
    -d '{"namespace":"myservice", "key":"user1", "rule":"5/10s", "algorithm":"AUTO", "cost":1}' > /dev/null
  echo -n "."
  sleep 0.2
done
