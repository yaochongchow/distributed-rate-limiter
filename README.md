# Distributed Rate Limiter

A high-performance, distributed rate limiting service built with Go and Redis. It uses a sliding window or token bucket algorithm to enforce rate limits across a cluster of service instances.

## 🚀 Quick Start

### 1. Start the Services
This project uses Docker Compose to run the Rate Limiter service, Redis Cluster, Envoy, Prometheus, and Grafana.

```bash
docker compose up -d --build
```
> **Note**: Prometheus is configured to run on port **9091** to avoid conflicts with common local services.

### 2. Scale the Rate Limiter
You can scale the rate limiter application to simulate a distributed environment:

```bash
docker compose up -d --scale ratelimiter=10
```

### 3. Generate Traffic
To populate metrics, run the included load generation script. This simulates traffic to the `webui` service, which forwards requests to the rate limiters.

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
    -d '{"namespace":"myservice", "key":"user1", "rule":"limit:5,window:10", "algorithm":"SLIDING_WINDOW", "cost":1}' > /dev/null
  echo -n "."
  sleep 0.2
done
```
</details>

## 📊 Observability

### Access Grafana
*   **URL**: [http://localhost:3000](http://localhost:3000)
    *   **User**: `admin`
    *   **Password**: `admin`

### Configure Dashboard
1.  **Add Data Source**:
    *   Go to **Connections > Data Sources > Add data source**.
    *   Select **Prometheus**.
    *   **URL**: `http://prometheus:9090` (internal docker DNS).
    *   Click **Save & test**.
2.  **Visualize Metrics**:
    *   **Request Rate**: `rate(ratelimiter_requests_total[1m])`
    *   **Latency**: `ratelimiter_allow_latency_seconds_bucket`

## 🏗 Architecture

The system consists of the following components:

*   **Rate Limiter Service (`/cmd/ratelimiter`)**: A GRPC service that handles `Allow()` requests. It executes Lua scripts on Redis to enforce limits atomically.
*   **Redis Cluster**: Stores the rate limit counters. Sharded to handle high throughput.
*   **Envoy**: Acts as a load balancer for the rate limiter instances.
*   **Web UI (`/cmd/webui`)**: A simple frontend and HTTP-to-GRPC gateway for testing.
*   **Prometheus & Grafana**: Scrapes metrics from the rate limiters for monitoring.

## 💻 Code Overview

### `internal/limiter`
Contains the core logic.
*   **`limiter.go`**: Manages the Redis connection and loads Lua scripts.
*   **Lua Scripts**:
    *   `token_bucket.lua`: Implements the Token Bucket algorithm.
    *   `sliding_window.lua`: Implements the Sliding Window algorithm.
*   **`redis_client.go`**: Helper to initialize the Redis Cluster client.

### `proto/ratelimit.proto`
Defines the GRPC API contract.
```protobuf
service RateLimitService {
  rpc Allow(AllowRequest) returns (AllowResponse);
}
```

## ⚠️ Troubleshooting

*   **Build Error `undefined: getIndexHTML`**: This has been fixed in the codebase by using the embedded `indexHTML` variable directly.
*   **Prometheus Port Conflict**: If port 9090 is in use, Prometheus is mapped to `9091` in `docker-compose.yml`. Use `localhost:9091` to access Prometheus directly.
