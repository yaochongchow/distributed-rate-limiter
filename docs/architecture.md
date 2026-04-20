# Architecture Diagram

This diagram reflects the current runtime architecture defined in `docker-compose.yml` and the service entrypoints under `cmd/` and `services/`.

```mermaid
flowchart LR
    User[Browser / Operator]

    subgraph Edge["Edge and Control Plane"]
        WebUI["Web UI\nGo HTTP app\n/api/allow + observability proxies"]
        Envoy["Envoy\n gRPC load balancer"]
        Debug["Debug Dashboard\nNode.js proxy + chaos service"]
        Streamer["Streamer\nRedis Streams consumer\nWebSocket fan-out"]
    end

    subgraph Compute["Rate-Limit Compute"]
        RL1["ratelimiter-1\nGo gRPC server"]
        RL2["ratelimiter-2\nGo gRPC server"]
        RLN["ratelimiter-N\nGo gRPC server"]
    end

    subgraph Data["Shared State"]
        Redis["Redis Cluster\n3 primaries + 3 replicas"]
        Events["Redis Stream\nrl:events"]
    end

    subgraph Obs["Observability Stack"]
        Prom["Prometheus"]
        Alert["Alertmanager"]
        Loki["Loki"]
        Promtail["Promtail"]
        Tempo["Tempo"]
        Grafana["Grafana"]
        DockerSock["Docker Socket"]
    end

    User -->|"HTTP UI"| WebUI
    User -->|"Dashboards"| Grafana
    User -->|"Live events via WebSocket"| Streamer

    WebUI -->|"Allow request via gRPC"| Envoy
    Envoy --> RL1
    Envoy --> RL2
    Envoy --> RLN

    RL1 -->|"Lua scripts\nToken Bucket / Sliding Window"| Redis
    RL2 -->|"Lua scripts\nToken Bucket / Sliding Window"| Redis
    RLN -->|"Lua scripts\nToken Bucket / Sliding Window"| Redis

    RL1 -.->|"XADD decision events"| Events
    RL2 -.->|"XADD decision events"| Events
    RLN -.->|"XADD decision events"| Events
    Streamer -->|"XREADGROUP / XACK"| Events
    Streamer -->|"Fan-out decision stream"| User

    WebUI -->|"Query metrics"| Prom
    WebUI -->|"Proxy health / alerts / logs / chaos"| Debug
    WebUI -->|"Read streamer stats"| Streamer

    Prom -->|"Scrape /metrics"| RL1
    Prom -->|"Scrape /metrics"| RL2
    Prom -->|"Scrape /metrics"| RLN
    Prom -->|"Alert rules"| Alert

    RL1 -->|"OTLP traces"| Tempo
    RL2 -->|"OTLP traces"| Tempo
    RLN -->|"OTLP traces"| Tempo
    WebUI -->|"OTLP traces"| Tempo

    RL1 -->|"stdout JSON logs"| Promtail
    RL2 -->|"stdout JSON logs"| Promtail
    RLN -->|"stdout JSON logs"| Promtail
    WebUI -->|"stdout JSON logs"| Promtail
    Streamer -->|"stdout JSON logs"| Promtail
    Promtail -->|"Ship logs"| Loki

    Debug -->|"PromQL"| Prom
    Debug -->|"Alert API"| Alert
    Debug -->|"LogQL"| Loki
    Debug -->|"Container status / kill / restore"| DockerSock

    Grafana -->|"Metrics"| Prom
    Grafana -->|"Logs"| Loki
    Grafana -->|"Traces"| Tempo
    Grafana -->|"Alerts"| Alert
```

## What Happens On A Request

1. The browser submits an `Allow` check to the `webui` HTTP endpoint.
2. `webui` forwards that check to `envoy` over gRPC.
3. `envoy` load-balances the call across the `ratelimiter` replicas.
4. The selected `ratelimiter` instance executes the token bucket or sliding window Lua script against the shared Redis Cluster.
5. The `ratelimiter` instance returns the decision to `webui`.
6. The same `ratelimiter` instance also publishes a non-blocking decision event into the `rl:events` Redis Stream.
7. `streamer` consumes that event and pushes it to connected browser clients over WebSocket.

## Observability And Operations

- `Prometheus` scrapes every `ratelimiter` replica and evaluates alert rules.
- `Alertmanager` receives those alerts and exposes the active alert state.
- `Promtail` tails container logs and ships them to `Loki`.
- `Tempo` receives OTLP traces from the Go services.
- `Grafana` is pre-provisioned with `Prometheus`, `Loki`, `Tempo`, and `Alertmanager`.
- `debug-dashboard` acts as the operational sidecar for the UI: it queries metrics, alerts, and logs, and it also talks to the Docker socket for chaos actions.
- `webui` is the single browser entrypoint for operators. It serves the UI, proxies operational APIs, queries Prometheus directly for the Decision Health charts, and calls the gRPC rate limiter through Envoy.
