#!/bin/bash
while true; do
  curl -s -X POST http://localhost:8080/api/allow \
    -d '{"namespace":"myservice", "key":"user1", "rule":"limit:5,window:10", "algorithm":"SLIDING_WINDOW", "cost":1}' > /dev/null
  echo -n "."
  sleep 0.2
done
