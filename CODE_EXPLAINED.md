# Codebase Explanation

This document provides a detailed walkthrough of the distributed rate limiter codebase, explaining the purpose and logic of each key file.

## `cmd/` - Entry Points

### `cmd/ratelimiter/main.go`
The main server binary.
*   **Purpose**: Starts the gRPC server and Prometheus metrics endpoint.
*   **Key Logic**:
    *   Connects to Redis Cluster using `limiter.NewRedisCluster`.
    *   Initialize the `Limiter` struct which loads Lua scripts.
    *   Registers the gRPC service (`RateLimitService`).
    *   **Metrics**: Defines `ratelimiter_requests_total` and `ratelimiter_allow_latency_seconds`.
    *   **Env Vars**: Reads `GRPC_ADDR`, `METRICS_ADDR`, and `REDIS_ADDRS` for configuration.

### `cmd/webui/main.go`
A simple web interface for testing.
*   **Purpose**: Provides a UI to manually send `Allow()` requests to the rate limiter.
*   **Key Logic**:
    *   Serves `index.html` at root (`/`).
    *   `/api/allow` endpoint: Accepts JSON, calls the gRPC `Allow()` method on the `ratelimiter` service, and returns the result.
    *   **Fix Applied**: Uses embedded `indexHTML` variable instead of undefined function.

## `internal/limiter/` - Core Logic

### `internal/limiter/limiter.go`
The main wrapper for rate limiting logic.
*   **Purpose**: Abstraction layer between Go code and Redis Lua scripts.
*   **Key Logic**:
    *   `New()`: Pre-loads the Lua scripts (`token_bucket.lua`, `sliding_window.lua`) into Redis for performance (`SCRIPT LOAD`).
    *   `TokenBucket()` & `SlidingWindow()`: Helper methods that execute the respective Lua scripts with the correct keys and arguments (capacity, rate, cost).

### `internal/limiter/redis_client.go`
*   **Purpose**: centralized Redis configuration.
*   **Key Logic**: Creates a `redis.ClusterClient` with connection pooling settings (pool size 300, min idle 50) optimized for high throughput.

### `internal/limiter/rules.go`
*   **Purpose**: Parses user-friendly rate limit strings into structured data.
*   **Logic**:
    *   Parses "1000rps" -> Token Bucket (Capacity=2000, Refill=1000/s).
    *   Parses "1000/1s" -> Sliding Window (Limit=1000, Window=1s).

### `internal/limiter/token_bucket.lua`
*   **Algorithm**: Token Bucket (Atomic).
*   **Logic**:
    1.  Retrieves current tokens and last refill timestamp (`HMGET`).
    2.  Calculates new tokens based on time elapsed (`delta * refill_rate`).
    3.  If `tokens >= cost`, allows request and decrements tokens.
    4.  Saves new state and updates expiration (`PEXPIRE`).
    5.  Returns allowed status, remaining tokens, and retry time.

### `internal/limiter/sliding_window.lua`
*   **Algorithm**: Sliding Window Log (Atomic).
*   **Logic**:
    1.  Removes old entries from the Sorted Set (`ZREMRANGEBYSCORE`) outside the window.
    2.  Check if `current_count + cost <= limit`.
    3.  If allowed, adds new entries (`ZADD`) with current timestamp as score.
    4.  Returns allowed status and remaining capacity.

## `proto/` - API Definition

### `proto/ratelimit.proto`
*   **Purpose**: Defines the contract between clients and the rate limiter.
*   **Service**: `RateLimitService` with a single RPC `Allow`.
*   **Messages**: `AllowRequest` (includes key, rule, cost) and `AllowResponse` (allowed bool, remaining, retry_after).

## `deploy/` - Infrastructure

### `deploy/envoy/envoy.yaml`
*   **Purpose**: Sidecar / Load Balancer configuration.
*   **Key Logic**:
    *   Listens on port `50051`.
    *   Route config `local_route` forwards all traffic to `ratelimiter_cluster`.
    *   `ratelimiter_cluster` uses Strict DNS to discover all `ratelimiter` container instances and load balances using Round Robin.

### `deploy/prometheus/prometheus.yml`
*   **Purpose**: Scrape configuration.
*   **Key Logic**: Configured to scrape the `ratelimiter` service on port `2112` every 5 seconds.

## Root Configuration

### `docker-compose.yml`
*   **Purpose**: Orchestrates the entire stack.
*   **Services**:
    *   `redis-1` to `redis-6`: 6 Redis nodes for the cluster.
    *   `redis-cluster-init`: One-off script to bond the nodes into a cluster.
    *   `ratelimiter`: The Go application (scaled to 10 instances).
    *   `webui`: The test frontend.
    *   `prometheus` & `grafana`: Observability stack.
    *   **Port Config**: Prometheus mapped to `9091` to avoid localhost conflict.
