# Distributed Rate Limiter Demo Guide

![Distributed Rate Limiter Demo](demo.gif)

## What This Project Is

This project is a **distributed rate limiter**.

A rate limiter is a system that decides whether a request should be:

- **allowed**, because it is still within the configured limit
- **denied**, because too many requests have already happened in a short period of time

Examples of where rate limiting is useful:

- protecting login endpoints from abuse
- limiting how fast one user can call an API
- controlling expensive background jobs
- keeping shared infrastructure stable under heavy traffic

The word **distributed** means the project is designed to work across **multiple service instances**, not just one server. All of those instances share the same decision state through Redis, so they can enforce the same limit consistently.

## Purpose Of This Project

The purpose of this project is to demonstrate how to build a practical distributed rate limiter with:

- **Go** for the backend service
- **Redis Cluster** for shared state
- **Envoy** for routing and load balancing
- **gRPC** for the service API
- **Prometheus and Grafana** for metrics and observability
- **a web UI** for learning, testing, and demoing the system

This project is useful as:

- a learning reference for distributed systems
- a demo for token bucket and sliding window algorithms
- a starter project for production-style rate limiting
- a local sandbox for testing traffic behavior and observability

## The Main Idea

When a client sends a request, the system asks:

> “Has this key used too much capacity already?”

If the answer is no, the request is allowed.

If the answer is yes, the request is denied and the system may also return how long to wait before retrying.

The project supports two common rate-limiting strategies:

- **Token bucket**
- **Sliding window**

## What The Demo Shows

The `demo.gif` in this folder shows the running web interface for the project.

It demonstrates:

- choosing common traffic scenarios
- building a valid rule
- submitting requests to the service
- viewing the decision returned by the backend
- understanding the request summary and algorithm simulator
- seeing recent request history
- watching live Prometheus-based charts update over time

## Core Concepts

### 1. Namespace

A **namespace** separates one group of limits from another.

Example:

- `api`
- `auth`
- `reports`

This prevents unrelated traffic from sharing the same rate limit.

### 2. Key

A **key** identifies the thing being limited.

Examples:

- a user ID
- a tenant ID
- an IP address
- an API client ID

If the key changes, the rate limit is tracked separately.

### 3. Rule

A **rule** defines the limit.

This project supports:

- `20rps`
  Meaning: token bucket, refilling at 20 requests per second
- `5/10s`
  Meaning: sliding window, allowing up to 5 requests every 10 seconds

Think of the rule as the sentence that tells the system:

- how much traffic is allowed
- over what period of time
- which style of rate limiting should be used

#### Rule Format 1: `20rps`

This means:

- `20` = the speed
- `rps` = requests per second

So `20rps` means:

> Add capacity at a speed of 20 requests every second.

This format is used for **token bucket**.

#### Rule Format 2: `5/10s`

This means:

- `5` = the maximum number of requests allowed
- `10s` = the time window

So `5/10s` means:

> Allow at most 5 requests during any 10-second period.

This format is used for **sliding window**.

#### Easy Way To Read A Rule

- `20rps` = "20 per second, burst-friendly"
- `5/10s` = "5 within 10 seconds, strict window"

### 4. Algorithm

The **algorithm** decides how the limit is enforced.

The UI supports:

- `AUTO`
- `TOKEN_BUCKET`
- `SLIDING_WINDOW`

`AUTO` is usually best, because the service can infer the correct algorithm from the rule format.

### 5. Cost

**Cost** means one request can consume more than one unit of budget.

Example:

- a normal request might cost `1`
- a heavy report job might cost `4`

This is useful when some operations are much more expensive than others.

Example:

- if the limit budget is `5`
- and one request has `cost = 3`

then that single request uses up 3 parts of the budget, not 1.

That means:

- the first heavy request may still be allowed
- but there may only be room for a small number of additional requests after that

This is why `cost` matters for expensive operations like report generation or data exports.

## A Very Simple Mental Model

Before looking at the algorithms, it helps to think of them like this:

- **token bucket** = a bucket that refills with tokens over time
- **sliding window** = a moving time box that counts recent requests

Both answer the same question:

> "Can this request still fit inside the allowed budget?"

They just answer it in different ways.

## The Two Algorithms

### Token Bucket

Token bucket is good when short bursts should be allowed, while still enforcing an average rate over time.

How it works:

- each key has a bucket of tokens
- tokens refill over time
- each request spends tokens
- if enough tokens exist, the request is allowed
- if not, the request is denied

Why it is useful:

- smooth for general API traffic
- allows short bursts
- efficient in Redis

#### Token Bucket In Plain English

Imagine a bucket with tokens in it.

- every request needs tokens
- if tokens are available, the request is allowed
- if not enough tokens are available, the request is denied
- the bucket slowly fills back up over time

So token bucket is good when it is okay for traffic to come in bursts for a short moment, as long as the long-term average stays reasonable.

#### Example: `20rps`

In this project, `20rps` means:

- the bucket refills at 20 tokens per second
- the bucket can also hold a burst of tokens

That means traffic might behave like this:

- a user is quiet for a while
- tokens refill in the background
- then the user suddenly sends several requests quickly
- those requests may still be allowed because saved-up tokens are available

So token bucket is not only asking:

> "How many requests happened this exact second?"

It is more like:

> "How much capacity is available right now after refill and previous usage?"

#### Why People Use Token Bucket

It feels more natural for many APIs because real traffic often comes in small bursts.

For example:

- a page loads and makes several requests at once
- a mobile app reconnects and sends several queued calls
- a user clicks around quickly for a few seconds

Token bucket handles that more gracefully than a strict hard window.

### Sliding Window

Sliding window is stricter.

How it works:

- the system looks at the recent time window
- it counts how many requests happened in that window
- if the new request would exceed the limit, it is denied

Why it is useful:

- strong for login or abuse prevention
- easier to reason about as “N requests per time window”
- stricter than token bucket for exact windows

#### Sliding Window In Plain English

Imagine the system is always looking backward over the most recent slice of time.

For example, with `5/10s`, it always asks:

> "How many requests happened in the last 10 seconds?"

If the answer is already 5, then the next request is denied.

If the answer is 4, then one more request can be allowed.

#### Example: `5/10s`

Suppose a user sends requests at these times:

- second 1
- second 2
- second 3
- second 4
- second 5

That is already 5 requests within a 10-second window.

If the same user sends another request at second 6:

- the system looks back over the last 10 seconds from that moment
- it still sees 5 recent requests
- so the new request is denied

Later, when the oldest request falls outside the last 10 seconds, capacity becomes available again.

For example:

- if the request from second 1 is now more than 10 seconds old
- it no longer counts inside the current window
- that opens room for a new request

#### Why Sliding Window Feels Stricter

Sliding window does not care that traffic was quiet before the burst.

It only cares about:

> "How many requests are inside the recent moving window right now?"

That is why it is often preferred for:

- login attempts
- password reset attempts
- OTP verification
- abuse-sensitive endpoints

It is easier to explain as a firm rule:

> "No more than 5 attempts in 10 seconds."

## Token Bucket vs Sliding Window

If the difference is still confusing, this is the simplest comparison:

- **token bucket**:
  "Capacity refills over time, so saved-up budget can be spent in a burst."
- **sliding window**:
  "Only a fixed number of recent requests may exist inside the moving time window."

Another way to think about it:

- token bucket is about **available balance**
- sliding window is about **recent count**

### Real-Life Analogy

Token bucket is like:

- a prepaid balance that slowly tops up again

Sliding window is like:

- a security rule that says "only 5 attempts are allowed in the last 10 minutes"

## Example Request

A request to this system usually contains:

```json
{
  "namespace": "auth",
  "key": "user123",
  "rule": "5/10s",
  "algorithm": "AUTO",
  "cost": 1
}
```

What each field means here:

- `namespace: "auth"`
  This is part of the application related to authentication.
- `key: "user123"`
  The limit applies to this specific user.
- `rule: "5/10s"`
  Allow at most 5 requests in 10 seconds.
- `algorithm: "AUTO"`
  Let the service infer the correct algorithm from the rule.
- `cost: 1`
  This request consumes 1 unit of budget.

## Example Response

An allowed response may look like:

```json
{
  "allowed": true,
  "remaining": 4,
  "retry_after_ms": 0,
  "algorithm_used": "sliding_window"
}
```

This means:

- the request was accepted
- 4 units of budget are still available
- there is no wait time because nothing was denied
- sliding window handled the request

A denied response may look like:

```json
{
  "allowed": false,
  "remaining": 0,
  "retry_after_ms": 3200,
  "algorithm_used": "sliding_window"
}
```

This means:

- the request was blocked
- no budget is currently left
- wait about 3.2 seconds before trying again
- sliding window handled the request

## What "Remaining" Really Means

`remaining` does **not** mean:

> "How many requests are allowed forever?"

It means:

> "How much budget is left right now after this decision?"

That budget can change quickly because:

- time passes
- tokens refill
- old requests fall out of the window
- new requests consume budget

So `remaining` is a current snapshot, not a permanent value.

## High-Level Request Flow

The project follows this path:

`Browser -> Web UI -> Envoy -> Rate Limiter -> Redis`

### Browser

The browser is where a user fills in the form and submits a request.

### Web UI

The web UI is both:

- a human-friendly interface
- a small HTTP gateway

It accepts JSON requests and forwards them to the gRPC service.

### Envoy

Envoy is a proxy and load balancer.

Its job is to route incoming gRPC traffic to the rate limiter service instances.

### Rate Limiter Service

This is the main backend written in Go.

It:

- validates input
- parses the rule
- chooses the correct algorithm
- calls Redis
- returns the final allow/deny decision
- records Prometheus metrics

### Redis Cluster

Redis stores the shared rate-limit state.

This is what makes the system distributed.

Even if there are multiple rate limiter instances, they all consult the same shared Redis-backed state.

## Why Redis Is Important Here

Without Redis, each application instance would keep its own local counters.

That would create a problem:

- instance A might think the request is safe
- instance B might think the same request is also safe

This would break the global rate limit.

Using Redis solves that by making the decision state shared and atomic.

## What The Web UI Is For

The web UI is designed to make the system easy to understand.

It helps by showing:

- scenario presets
- a rule builder
- validation messages
- request summaries
- Redis key preview
- algorithm simulator
- response explanations
- recent request timeline
- observability charts

This makes it much easier to learn the system than using only raw API calls.

## What The Charts Mean

The observability section uses Prometheus data.

Main charts:

- **Request rate**
  Shows how much traffic is hitting the service
- **Allowed vs denied**
  Shows how many requests are being accepted versus rejected
- **P95 latency**
  Shows slower request behavior, not just the average

These charts help answer questions like:

- Is traffic rising?
- Are limits actually being triggered?
- Is the service staying fast under load?

## What Grafana And Prometheus Are Doing

### Prometheus

Prometheus collects metrics from the rate limiter service.

It stores time-series data such as:

- request counts
- allow/deny counts
- latency
- service health

### Grafana

Grafana is the dashboard layer.

It reads Prometheus data and turns it into graphs and dashboards.

In this project, the web UI also shows a lightweight built-in observability view, while Grafana provides a more traditional monitoring interface.

## What Makes This A Good Demo Project

This project combines several ideas that are often explained separately:

- distributed coordination
- Redis-backed atomic state
- API design with gRPC
- load balancing with Envoy
- live metrics with Prometheus and Grafana
- a friendly frontend for experimentation

That makes it useful for both engineering learning and technical demos.

## Typical Use Cases

This kind of system can protect:

- login endpoints
- public APIs
- internal APIs
- billing-sensitive operations
- report generation
- OTP or verification flows
- webhooks

## What A Response Means

A response usually includes:

- `allowed`
- `remaining`
- `retry_after_ms`
- `algorithm_used`

Meaning:

- `allowed`: whether the request passed
- `remaining`: how much budget is left after this decision
- `retry_after_ms`: how long to wait before retrying after denial
- `algorithm_used`: which algorithm handled the request

## In Simple Terms

If someone asks, “What does this project do?”, the short answer is:

> It is a Redis-backed distributed service that decides whether a request should be allowed or blocked based on rate-limit rules, and it includes a UI and observability stack to make the behavior easy to test and understand.

## Files In This Folder

- `demo.gif`
  A visual walkthrough of the running project
- `README.md`
  This explanation document

## Suggested Viewing Order

If this project is new, the best order is:

1. Watch `demo.gif`
2. Read this file
3. Open the main project `README.md`
4. Run the stack locally
5. Try different rules in the web UI

## Final Summary

This project exists to show how a modern distributed rate limiter works end to end:

- request comes in
- rule is parsed
- Redis stores shared state
- algorithm decides allow or deny
- metrics are collected
- the UI helps explain what happened

It is both a working system and a teaching tool.
