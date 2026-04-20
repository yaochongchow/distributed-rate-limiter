# Distributed Rate Limiter

A high-performance, distributed rate limiting service built with Go and Redis. Enforces per-key limits across a cluster of service instances using atomic Lua scripts — with real-time event streaming, full observability, and chaos engineering built in.

![Distributed Rate Limiter Demo](artifacts/demo.gif)

## Overview

This project exposes a gRPC `Allow` API backed by Redis Cluster and wraps it with a complete operational platform:

- **Rate Limiter** — gRPC service enforcing token bucket and sliding window limits atomically via Lua scripts
- **Web UI** — unified browser interface: rate-limit playground, live event feed, service health, alerts, logs, and chaos controls — all in one page
- **Event Streamer** — Redis Streams consumer that fans out real-time rate-limit decisions to WebSocket clients
- **Debug Dashboard** — Node.js service backing the web UI's health, alert, log, and chaos proxy endpoints
- **Envoy** — gRPC load balancer across rate limiter replicas
- **Redis Cluster** — 6-node cluster (3 primaries, 3 replicas) for distributed shared state
- **Prometheus + AlertManager** — metrics scraping and alert routing with pre-configured rules
- **Grafana** — auto-provisioned dashboards with Prometheus, Loki, and Tempo data sources
- **Loki + Promtail** — log aggregation; Promtail tails Docker container logs and ships structured JSON fields to Loki
- **Tempo** — distributed tracing backend receiving OTLP spans from Go services

See the full architecture diagram in [docs/architecture.md](docs/architecture.md).

## Quick Start

### 1. Start the stack

```bash
docker compose up -d --build
```

The Redis cluster initializes automatically. All other services wait for it before starting.

### 2. Access the services

| Service | URL | Notes |
|---|---|---|
| **Web UI** | http://localhost:8080 | All-in-one interface — playground, events, health, alerts, logs, chaos |
| Event Stream | http://localhost:8888 | Standalone real-time WebSocket dashboard |
| Debug Dashboard | http://localhost:4001 | Standalone debug service UI |
| Grafana | http://localhost:3000 | Pre-provisioned dashboards — login `admin` / `admin` |
| Prometheus | http://localhost:9091 | Raw metrics and query explorer |
| AlertManager | http://localhost:9093 | Active alerts and silences |
| Envoy Admin | http://localhost:9901 | Proxy stats and cluster health |
| gRPC endpoint | `localhost:50051` | Direct gRPC access via Envoy |

> **Port conflicts**: any host port can be overridden at startup, for example `WEBUI_PORT=8081 GRAFANA_PORT=3001 DEBUG_DASHBOARD_PORT=4002 docker compose up -d --build`.

### 3. Scale the rate limiter

```bash
docker compose up -d --scale ratelimiter=3
```

Envoy automatically load-balances across all instances. The live event feed in the web UI shows the `instance` field on each decision so you can see which replica handled each request.

### 4. Generate traffic

```bash
chmod +x ./generate_traffic.sh
./generate_traffic.sh
```

This sends ~5 requests/sec through the web UI gateway. Open the web UI at http://localhost:8080 and watch decisions appear in the Live Event Feed in real time.

<details>
<summary>generate_traffic.sh content</summary>

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

## Web UI

The web UI at http://localhost:8080 is a single page that surfaces every part of the system. It is the primary interface for the project.

### Rate-Limit Playground

Submit `Allow` requests interactively:

- **Scenario presets** — one-click token bucket, sliding window, and heavy-cost examples
- **Rule builder** — fills the rule field from human-readable inputs; syncs back when you type directly
- **Validation** — explains the rule in plain language, previews the Redis key, and blocks invalid submissions
- **Algorithm comparison** — shows side-by-side projections for both algorithms under the current parameters
- **Response explanation** — interprets `allowed`, `remaining`, and `retry_after_ms` in plain language
- **Request history** — persisted to `localStorage`; shows a local timeline chart of past requests
- **Usage examples** — auto-generated `curl` and JSON snippets for the current form values

### Observability Charts

Live metrics from Prometheus, refreshed every 5 seconds:

- Total request rate, allowed rate, denied rate, p95 latency
- 10-minute time series charts: request rate, allowed vs denied, p95 latency
- Direct links to Grafana, Prometheus, AlertManager, Envoy Admin, and the streamer

### Service Health

Compact status panel showing a green/red dot for every backend component: Rate Limiter (with instance count), Prometheus, AlertManager, and Loki. Refreshed every 5 seconds via the `/api/service-health` proxy endpoint.

### Live Event Feed

WebSocket connection to the streamer that shows every rate-limit decision as it happens:

| Column | Description |
|---|---|
| Time | Local timestamp |
| Namespace | Request namespace |
| Key | Rate-limit key |
| Algorithm | `TB` (token bucket) or `SW` (sliding window) pill |
| Result | `Allowed` or `Denied` pill |
| Remaining | Tokens or slots left after this request |
| Latency | End-to-end decision time |
| Instance | Hostname of the ratelimiter replica (first 12 chars) |

Filter by result (all / allowed only / denied only), clear the table, and watch the event counter increment with every decision. The connection reconnects automatically if dropped.

### Active Alerts

Polls AlertManager every 15 seconds via `/api/active-alerts`. Each firing alert shows its name, severity badge (critical / warning / info), annotation summary, and start time.

### Recent Logs

Dark terminal-style log viewer pulling from Loki every 15 seconds via `/api/recent-logs`. Select the service (`ratelimiter`, `webui`, or `streamer`) from a dropdown. Each line shows:

- Local timestamp
- Color-coded level badge (`INFO` in teal, `WARN` in amber, `ERRO` in red)
- Message
- Parsed fields: `ns=`, `key=`, `algo=`, `allowed=`, latency

### Chaos Engineering

Buttons to deliberately kill containers and observe fault tolerance:

| Button | Action |
|---|---|
| Kill Rate Limiter | Stops a random ratelimiter container |
| Kill Redis Node | Stops a random redis-1 through redis-6 container |
| Restore All | Starts every exited compose container |

A container status table below the buttons shows the current state of every compose service, refreshed every 10 seconds. The web UI proxies all chaos calls to the debug-dashboard's Docker socket API so the browser never needs to know the debug service's port.

## API

The service contract is defined in [`proto/ratelimit.proto`](proto/ratelimit.proto):

```protobuf
service RateLimitService {
  rpc Allow(AllowRequest) returns (AllowResponse);
}
```

**Request fields**

| Field | Type | Description |
|---|---|---|
| `namespace` | string | Isolation scope (e.g. `"api"`, `"payments"`) |
| `key` | string | Per-entity identifier (e.g. user ID, IP address) |
| `rule` | string | Limit rule — see formats below |
| `algorithm` | enum | `TOKEN_BUCKET`, `SLIDING_WINDOW`, or `AUTO` |
| `cost` | int64 | Tokens to consume per request (default `1`) |

**Rule formats**

| Format | Algorithm | Example | Meaning |
|---|---|---|---|
| `{N}rps` | Token Bucket | `100rps` | 100 tokens/sec, burst = 200 |
| `{N}/{D}s` | Sliding Window | `5/10s` | 5 requests per 10-second window |

**Response fields**

| Field | Type | Description |
|---|---|---|
| `allowed` | bool | Whether this request is permitted |
| `remaining` | int64 | Tokens or slots remaining after this request |
| `retry_after_ms` | int64 | Milliseconds until a denied request can retry |
| `algorithm_used` | string | Which algorithm was applied |

**Example via HTTP gateway**

```bash
curl -X POST http://localhost:8080/api/allow \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"api","key":"user123","rule":"20rps","algorithm":"AUTO","cost":1}'
```

**Web UI proxy endpoints** (all served from port 8080)

| Endpoint | Method | Proxies to |
|---|---|---|
| `/api/service-health` | GET | debug-dashboard `/api/health` |
| `/api/active-alerts` | GET | debug-dashboard `/api/alerts` |
| `/api/recent-logs` | GET | debug-dashboard `/api/logs` |
| `/api/stream-stats` | GET | streamer `/api/stats` |
| `/api/chaos/status` | GET | debug-dashboard `/api/chaos/status` |
| `/api/chaos/kill-ratelimiter` | POST | debug-dashboard `/api/chaos/kill-ratelimiter` |
| `/api/chaos/kill-redis` | POST | debug-dashboard `/api/chaos/kill-redis` |
| `/api/chaos/restore` | POST | debug-dashboard `/api/chaos/restore` |

## Real-time Event Streaming

Every rate-limit decision is published asynchronously to a Redis Stream (`rl:events`). The `streamer` service consumes this stream using a consumer group for fault-tolerant delivery and fans events out to all connected WebSocket clients — including the web UI's Live Event Feed.

**How it works**

1. `ratelimiter` calls `XADD rl:events` after each `Allow()` (non-blocking, fire-and-forget)
2. `streamer` runs `XREADGROUP` with a 1-second block, ACKs each entry after delivery
3. On restart, `streamer` reclaims pending entries idle > 30 seconds via `XCLAIM`
4. Each WebSocket client gets a 256-event buffer; slow consumers are evicted rather than blocking the fan-out

The standalone event dashboard at http://localhost:8888 supports filtering by namespace, key, algorithm, and result. The web UI connects to the same WebSocket endpoint directly from the browser.

**Event payload fields**

| Field | Description |
|---|---|
| `ts` | Unix millisecond timestamp |
| `ns` | Namespace |
| `key` | Rate-limit key |
| `algo` | `tb` (token bucket) or `sw` (sliding window) |
| `allowed` | `true` / `false` |
| `remaining` | Tokens/slots remaining |
| `retry_ms` | Retry-after in milliseconds |
| `latency_us` | End-to-end latency in microseconds |
| `instance` | Hostname of the ratelimiter replica |

## Observability

### Metrics — Prometheus + Grafana

Grafana is fully auto-provisioned: data sources (Prometheus, Loki, Tempo, AlertManager) and the Rate Limiter Overview dashboard are loaded at startup — no manual setup needed.

- **Grafana**: http://localhost:3000 — login `admin` / `admin`
- **Prometheus**: http://localhost:9091

Useful queries:

```promql
# Request rate
sum(rate(ratelimiter_requests_total[1m]))

# Allow / deny split
sum by (allowed) (rate(ratelimiter_requests_total[1m]))

# p95 latency
histogram_quantile(0.95, sum by (le) (rate(ratelimiter_allow_latency_seconds_bucket[5m])))

# Per-instance request rate
sum by (instance) (rate(ratelimiter_requests_total[1m]))
```

### Logs — Loki + Promtail

All Go services emit structured JSON logs (zerolog). Promtail discovers Docker containers via the Docker socket and ships logs to Loki, parsing fields like `level`, `trace_id`, `namespace`, and `algo` as labels.

Query logs in Grafana using LogQL:

```logql
{service="ratelimiter"} | json | allowed="false"
{service="webui"} | json | latency_us > 1000
```

### Distributed Tracing — Tempo

Both `ratelimiter` and `webui` export OTLP spans to Tempo. The gRPC stats handler (`otelgrpc`) propagates trace context automatically so the full request path — HTTP → gRPC → Redis — appears as a single trace.

Traces are viewable in Grafana under **Explore → Tempo**. The Grafana provisioning includes:
- trace-to-logs correlation (click a span → jump to matching Loki logs)
- trace-to-metrics correlation (click a span → jump to Prometheus)

### Alerts — AlertManager

Pre-configured alert rules in [`deploy/prometheus/alerts.yml`](deploy/prometheus/alerts.yml):

| Alert | Condition | Severity |
|---|---|---|
| `HighDenyRate` | >50% requests denied for 30s | warning |
| `HighLatencyP99` | p99 latency >100ms for 1m | warning |
| `RateLimiterInstanceDown` | Any instance unreachable for 30s | critical |
| `NoRateLimiterInstances` | Zero instances up for 10s | critical |
| `RequestRateSpike` | >5000 req/sec | warning |

AlertManager routes to the debug dashboard webhook receiver. Firing alerts appear in the web UI's Active Alerts panel in real time.

## Architecture

```
Browser → http://localhost:8080 (Web UI)
  │
  │  POST /api/allow ──────────→ Envoy (:50051) ──→ Rate Limiter × N ──→ Redis Cluster
  │                                                        │
  │                                                  XADD rl:events
  │                                                        │
  │  WS ws://localhost:8888 ←── Streamer ←── XREADGROUP ──┘
  │  (Live Event Feed panel)
  │
  │  GET /api/service-health ──→ [webui proxy] ──→ debug-dashboard:4000/api/health
  │  GET /api/active-alerts  ──→ [webui proxy] ──→ debug-dashboard:4000/api/alerts
  │  GET /api/recent-logs    ──→ [webui proxy] ──→ debug-dashboard:4000/api/logs
  │  POST /api/chaos/*       ──→ [webui proxy] ──→ debug-dashboard:4000/api/chaos/*
  │                                                        │
  │                                               Docker socket (chaos)
  │
  └── GET /api/observability ──→ Prometheus (:9091)

Grafana (:3000)
  ← Prometheus (:9091) ← rate limiter /metrics
  ← Loki (:3100)       ← Promtail ← Docker logs
  ← Tempo (:3200)      ← OTLP from ratelimiter + webui
  ← AlertManager (:9093)
```

## Code Overview

### `cmd/ratelimiter`

Core gRPC service. Each `Allow()` call:
1. Parses and validates the rule string
2. Executes the appropriate Lua script atomically on Redis
3. Records Prometheus counter + histogram
4. Emits a zerolog structured log line
5. Publishes a `StreamEvent` to Redis Streams asynchronously

### `cmd/webui`

HTTP server and all-in-one browser UI. Backend endpoints:

| Endpoint | Description |
|---|---|
| `GET /` | Embedded single-page UI |
| `POST /api/allow` | JSON → gRPC proxy to rate limiter via Envoy |
| `GET /api/observability` | Aggregated Prometheus queries for the charts panel |
| `GET /api/service-health` | Proxies debug-dashboard health check |
| `GET /api/active-alerts` | Proxies AlertManager alerts via debug-dashboard |
| `GET /api/recent-logs` | Proxies Loki log query via debug-dashboard |
| `GET /api/stream-stats` | Proxies streamer stats |
| `GET /api/chaos/status` | Proxies container status via debug-dashboard |
| `POST /api/chaos/*` | Proxies chaos actions via debug-dashboard |

### `cmd/streamer`

Redis Streams → WebSocket fan-out service. Provides:
- `GET /` — standalone event stream dashboard (dark-theme table with filters)
- `GET /ws/events` — WebSocket endpoint; clients receive every `rl:events` entry in real time
- `GET /api/stats` — connected clients, buffer usage

Consumer group `streamer` with pending-entry reclaim on restart for exactly-once delivery guarantees.

### `services/debug-dashboard`

Node.js + Express service. Backs the web UI's proxy endpoints and AlertManager webhook:
- `GET /api/health` — aggregated health from Prometheus, AlertManager, Loki
- `GET /api/metrics/summary` — instant Prometheus query results
- `GET /api/alerts` — proxies AlertManager `/api/v2/alerts`
- `GET /api/logs` — Loki LogQL query with JSON field parsing
- `POST /api/alertmanager/webhook` — receives alert notifications; fans out to WS clients
- `POST /api/chaos/kill-ratelimiter` — stops a random ratelimiter container via Docker socket
- `POST /api/chaos/kill-redis` — stops a random Redis node
- `POST /api/chaos/restore` — starts all exited compose containers
- `GET /api/chaos/status` — lists container states
- `WS /ws/alerts` — pushes current alerts on connect, then streams metrics every 3 seconds

### `internal/limiter`

- **`limiter.go`** — `Limiter` struct; `TokenBucket()` and `SlidingWindow()` load and execute Lua scripts
- **`rules.go`** — `ParseRule()` converts `"20rps"` and `"5/10s"` into a `ParsedRule` struct
- **`token_bucket.lua`** — atomic refill-and-consume via `HGETALL` / `HSET`
- **`sliding_window.lua`** — atomic sliding window via sorted sets (`ZADD` / `ZREMRANGEBYSCORE` / `ZCARD`)
- **`redis_client.go`** — Redis Cluster client initialization from comma-separated address list

## Configuration

All services are configured via environment variables. Key variables:

| Service | Variable | Default | Description |
|---|---|---|---|
| ratelimiter | `REDIS_ADDRS` | `redis-1:7001,...` | Comma-separated Redis Cluster nodes |
| ratelimiter | `GRPC_ADDR` | `0.0.0.0:50051` | gRPC listen address |
| ratelimiter | `METRICS_ADDR` | `0.0.0.0:2112` | Prometheus metrics endpoint |
| ratelimiter | `OTEL_EXPORTER_OTLP_ENDPOINT` | _(unset)_ | Tempo OTLP endpoint; omit to disable tracing |
| webui | `GRPC_TARGET` | `envoy:50051` | gRPC upstream (Envoy) |
| webui | `HTTP_ADDR` | `0.0.0.0:8080` | HTTP listen address |
| webui | `PROMETHEUS_URL` | `http://prometheus:9090` | Prometheus base URL for observability queries |
| webui | `DEBUG_DASHBOARD_URL` | `http://debug-dashboard:4000` | Debug dashboard base URL for proxy endpoints |
| webui | `STREAMER_URL` | `http://streamer:8888` | Streamer base URL for stats proxy |
| streamer | `REDIS_ADDRS` | `redis-1:7001,...` | Comma-separated Redis Cluster nodes |
| streamer | `HTTP_ADDR` | `0.0.0.0:8888` | HTTP + WebSocket listen address |
| debug-dashboard | `PROMETHEUS_URL` | `http://prometheus:9090` | Prometheus base URL |
| debug-dashboard | `ALERTMANAGER_URL` | `http://alertmanager:9093` | AlertManager base URL |
| debug-dashboard | `LOKI_URL` | `http://loki:3100` | Loki base URL |

Host ports can all be overridden via `docker compose` environment variables:

```bash
WEBUI_PORT=8081 GRAFANA_PORT=3001 PROMETHEUS_PORT=9092 DEBUG_DASHBOARD_PORT=4002 docker compose up -d --build
```

## Troubleshooting

- **Port conflict** — override the host port with the corresponding `*_PORT` variable (see table above).
- **Redis cluster not forming** — the `redis-cluster-init` container retries until all six nodes respond to `PING`. If it exits with an error, check `docker compose logs redis-cluster-init`.
- **Charts look empty right after startup** — Prometheus scrapes every 5 seconds; wait a moment or run `generate_traffic.sh` to produce data.
- **Live Event Feed shows "Unavailable"** — the browser connects directly to the streamer WebSocket on port 8888. Ensure that port is reachable from your browser (not blocked by a firewall or remapped).
- **Service Health shows all red** — the debug-dashboard may still be starting. Check `docker compose logs debug-dashboard`. Health refreshes automatically every 5 seconds.
- **Recent Logs show nothing** — Loki and Promtail need a few seconds to ingest logs after startup. Generate some traffic with `generate_traffic.sh` and wait ~10 seconds.
- **Chaos buttons return errors** — Docker socket must be mounted into the debug-dashboard container. Check that `/var/run/docker.sock` exists on the host and that the compose file mounts it.
- **Tempo traces missing** — ensure `OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4318` is set. The Go services log a warning if the variable is unset.
- **Grafana datasource errors** — Grafana provisions sources at boot. If Prometheus or Loki aren't ready yet, reload the datasource from **Connections > Data Sources**.
- **Slow consumer eviction** — if the event stream client buffer (256 events) fills up, the WebSocket connection is closed. Reconnect automatically happens after 2 seconds.
