FROM golang:1.25-alpine AS build
WORKDIR /app
RUN apk add --no-cache git

# Dependencies first (better caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy everything and build
COPY . .

# Build all three binaries
RUN CGO_ENABLED=0 go build -o /bin/ratelimiter ./cmd/ratelimiter
RUN CGO_ENABLED=0 go build -o /bin/webui       ./cmd/webui
RUN CGO_ENABLED=0 go build -o /bin/streamer    ./cmd/streamer

# Small runtime image
FROM alpine:3.19
RUN adduser -D appuser
USER appuser

COPY --from=build /bin/ratelimiter /ratelimiter
COPY --from=build /bin/webui       /webui
COPY --from=build /bin/streamer    /streamer

EXPOSE 50051 2112 8080 8888

# Default (compose will override for other services)
ENTRYPOINT ["/ratelimiter"]
