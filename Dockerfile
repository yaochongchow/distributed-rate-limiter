FROM golang:1.25-alpine AS build
WORKDIR /app
RUN apk add --no-cache git

# Dependencies first (better caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy everything and build
COPY . .

# Build both binaries
RUN CGO_ENABLED=0 go build -o /bin/ratelimiter ./cmd/ratelimiter
RUN CGO_ENABLED=0 go build -o /bin/webui ./cmd/webui

# Small runtime image
FROM alpine:3.19
RUN adduser -D appuser
USER appuser

COPY --from=build /bin/ratelimiter /ratelimiter
COPY --from=build /bin/webui /webui

EXPOSE 50051 2112 8080

# Default container command (compose will override for webui service)
ENTRYPOINT ["/ratelimiter"]
