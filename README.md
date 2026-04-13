# Distributed Rate Limiter

A high-performance, distributed rate limiting service built with Go and Redis. It uses a sliding window or token bucket algorithm to enforce rate limits across a cluster of service instances.

![Distributed Rate Limiter Demo](artifacts/demo.gif)

## Overview

This project exposes a gRPC `Allow` API backed by Redis and wraps it with:

- a browser UI for building and submitting rate-limit requests
- Envoy as the gRPC entrypoint and load balancer
- Prometheus and Grafana for live metrics
- Redis Cluster for shared distributed state

The UI includes:

- scenario presets and a rule builder
- request validation and plain-language response explanations
- collapsible guidance for request fields and algorithm selection
- Redis key preview and algorithm comparison
- recent request history and a local request timeline
- live Prometheus-backed charts with direct links to Grafana, Prometheus, and Envoy

## Quick Start

### 1. Start the Services
This project uses Docker Compose to run the rate limiter service, Redis Cluster, Envoy, Prometheus, Grafana, and the web UI.

```bash
docker compose up -d --build
```

After startup:

- Web UI: [http://localhost:8080](http://localhost:8080)
- Grafana: [http://localhost:3000](http://localhost:3000)
- Prometheus: [http://localhost:9091](http://localhost:9091)
- Envoy Admin: [http://localhost:9901](http://localhost:9901)
- gRPC endpoint: `localhost:50051`

> **Note**: Prometheus is mapped to host port `9091` to avoid common local conflicts.
> **Tip**: If host ports like `8080` or `50051` are already in use, override them when starting the stack, for example `WEBUI_PORT=8081 docker compose up -d --build`.

### 2. Scale the Rate Limiter
You can scale the rate limiter application to simulate a distributed environment:

```bash
docker compose up -d --scale ratelimiter=10
```

### 3. Generate Traffic
To populate metrics, run the included load generation script. This sends requests to the `webui` gateway, which forwards them through Envoy to the rate limiter service.

```bash
# Make the script executable first
chmod +x ./generate_traffic.sh 

# Run it
./generate_traffic.sh
```
*(Note: If you don't have this script locally, create one with the content below)*:
<details>
<summary>Click to see generate_traffic.sh content</summary>

```bash
#!/bin/bash
while true; do
  curl -s -X POST http://localhost:8080/api/allow \
    -H 'Content-Type: application/json' \
    -d '{"namespace":"myservice", "key":"user1", "rule":"5/10s", "algorithm":"AUTO", "cost":1}' > /dev/null
  echo -n "."
  sleep 0.2
done
```
</details>

## API

The public service contract is defined in [`proto/ratelimit.proto`](proto/ratelimit.proto):

```protobuf
service RateLimitService {
  rpc Allow(AllowRequest) returns (AllowResponse);
}
```

Example HTTP request through the web UI gateway:

```bash
curl -X POST http://localhost:8080/api/allow \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"api","key":"user123","rule":"20rps","algorithm":"AUTO","cost":1}'
```

Supported rule formats:

- `20rps` for token bucket
- `5/10s` for sliding window

## Observability

### Access Grafana

- URL: [http://localhost:3000](http://localhost:3000)
- User: `admin`
- Password: `admin`

### Prometheus

- URL: [http://localhost:9091](http://localhost:9091)
- Example request-rate query: `sum(rate(ratelimiter_requests_total[1m]))`
- Example latency query: `histogram_quantile(0.95, sum by (le) (rate(ratelimiter_allow_latency_seconds_bucket[5m])))`

The web UI also exposes a built-in observability view with:

- total request rate
- allowed and denied rate
- p95 latency
- recent local request history
- direct host links to Grafana, Prometheus, and Envoy

## Architecture

The system consists of the following components:

- **Rate Limiter Service (`/cmd/ratelimiter`)**: gRPC service that handles `Allow()` requests and executes Redis Lua scripts atomically.
- **Redis Cluster**: shared distributed state for token bucket and sliding-window data.
- **Envoy**: load balances gRPC traffic across rate limiter instances.
- **Web UI (`/cmd/webui`)**: browser UI plus HTTP-to-gRPC gateway for testing and demos.
- **Prometheus and Grafana**: scrape and visualize service metrics.

Request flow:

`Browser -> Web UI -> Envoy -> Rate Limiter -> Redis`

## Code Overview

### `internal/limiter`
Contains the core logic.

- **`limiter.go`**: manages Redis script execution.
- **`token_bucket.lua`**: token bucket enforcement.
- **`sliding_window.lua`**: sliding-window enforcement.
- **`redis_client.go`**: Redis Cluster client setup.
- **`rules.go`**: parses rule formats such as `20rps` and `5/10s`.

## Troubleshooting

- **Port conflict**: if `8080`, `50051`, or another host port is already in use, override the published port when starting the stack.
- **Prometheus host port**: Prometheus is mapped to `9091`, so use `localhost:9091` on the host.
- **Metrics look sparse right after startup**: Prometheus scrapes every 5 seconds, so charts need a little time to fill in.
