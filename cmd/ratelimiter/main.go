package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"
	"example.com/distributed-rate-limiter/internal/limiter"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// StreamEvent is a single rate-limit decision published to Redis Streams.
type StreamEvent struct {
	Timestamp  int64
	Namespace  string
	Key        string
	Algo       string
	Allowed    bool
	Remaining  int64
	RetryMS    int64
	LatencyUS  int64
	InstanceID string
}

// StreamPublisher asynchronously publishes events to Redis Streams.
// It uses a buffered channel so the gRPC handler is never blocked.
// Events are dropped silently when the buffer is full (backpressure
// protection: the rate limiter's correctness must never depend on
// stream delivery).
type StreamPublisher struct {
	rdb    *redis.ClusterClient
	ch     chan StreamEvent
	stopCh chan struct{}
}

func newStreamPublisher(rdb *redis.ClusterClient) *StreamPublisher {
	p := &StreamPublisher{
		rdb:    rdb,
		ch:     make(chan StreamEvent, 4096),
		stopCh: make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *StreamPublisher) publish(e StreamEvent) {
	select {
	case p.ch <- e:
	default:
		// buffer full — drop rather than block the gRPC handler
	}
}

func (p *StreamPublisher) stop() { close(p.stopCh) }

func (p *StreamPublisher) run() {
	for {
		select {
		case e := <-p.ch:
			allowed := "0"
			if e.Allowed {
				allowed = "1"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			err := p.rdb.XAdd(ctx, &redis.XAddArgs{
				Stream: "rl:events",
				MaxLen: 50000,
				Approx: true,
				Values: map[string]interface{}{
					"ts":         e.Timestamp,
					"ns":         e.Namespace,
					"key":        e.Key,
					"algo":       e.Algo,
					"allowed":    allowed,
					"remaining":  e.Remaining,
					"retry_ms":   e.RetryMS,
					"latency_us": e.LatencyUS,
					"instance":   e.InstanceID,
				},
			}).Err()
			cancel()
			if err != nil {
				log.Warn().Err(err).Msg("stream publish failed")
			}
		case <-p.stopCh:
			return
		}
	}
}

// initTracer sets up an OTLP HTTP trace exporter pointing at Tempo.
// If OTEL_EXPORTER_OTLP_ENDPOINT is unset the function is a no-op so
// the service starts cleanly without Tempo.
func initTracer(ctx context.Context, serviceName, instanceID string) (func(context.Context) error, error) {
	endpoint := env("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	res, _ := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceInstanceID(instanceID),
		),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

type srv struct {
	ratelimitv1.UnimplementedRateLimitServiceServer
	lim        *limiter.Limiter
	pub        *StreamPublisher
	instanceID string

	reqs *prometheus.CounterVec
	lat  *prometheus.HistogramVec
}

func newSrv(l *limiter.Limiter, pub *StreamPublisher, instanceID string) *srv {
	reqs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ratelimiter_requests_total",
		Help: "Total Allow requests",
	}, []string{"allowed", "algo"})
	lat := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ratelimiter_allow_latency_seconds",
		Help:    "Allow() latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"algo"})

	prometheus.MustRegister(reqs, lat)
	return &srv{lim: l, pub: pub, instanceID: instanceID, reqs: reqs, lat: lat}
}

// redisKey builds a Redis cluster key with hash tags so one client key
// always lands on the same slot.
func redisKey(ns, key, suffix string) string {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		ns = "default"
	}
	return fmt.Sprintf("rl:{%s:%s}:%s", ns, key, suffix)
}

func (s *srv) Allow(ctx context.Context, req *ratelimitv1.AllowRequest) (*ratelimitv1.AllowResponse, error) {
	start := time.Now()

	key := strings.TrimSpace(req.GetKey())
	rule := strings.TrimSpace(req.GetRule())
	if key == "" || rule == "" {
		return nil, status.Error(codes.InvalidArgument, "key and rule required")
	}
	cost := req.GetCost()
	if cost <= 0 {
		cost = 1
	}

	parsed, err := limiter.ParseRule(rule)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	algo := parsed.Algo
	switch req.GetAlgorithm() {
	case ratelimitv1.Algorithm_TOKEN_BUCKET:
		if parsed.Algo != limiter.AlgoTB {
			return nil, status.Errorf(codes.InvalidArgument, "algorithm TOKEN_BUCKET does not match rule %q", rule)
		}
		algo = limiter.AlgoTB
	case ratelimitv1.Algorithm_SLIDING_WINDOW:
		if parsed.Algo != limiter.AlgoSW {
			return nil, status.Errorf(codes.InvalidArgument, "algorithm SLIDING_WINDOW does not match rule %q", rule)
		}
		algo = limiter.AlgoSW
	case ratelimitv1.Algorithm_ALGO_UNSPECIFIED:
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported algorithm")
	}

	switch algo {
	case limiter.AlgoTB:
		if cost > parsed.Capacity {
			return nil, status.Errorf(codes.InvalidArgument, "cost %d exceeds token bucket capacity %d", cost, parsed.Capacity)
		}
	case limiter.AlgoSW:
		if cost > parsed.Limit {
			return nil, status.Errorf(codes.InvalidArgument, "cost %d exceeds sliding window limit %d", cost, parsed.Limit)
		}
	}

	var res limiter.Result
	switch algo {
	case limiter.AlgoTB:
		res, err = s.lim.TokenBucket(ctx,
			redisKey(req.GetNamespace(), key, "tb"),
			parsed.Capacity, parsed.RefillPerSec, parsed.TTL, cost)
	case limiter.AlgoSW:
		res, err = s.lim.SlidingWindow(ctx,
			redisKey(req.GetNamespace(), key, "sw"),
			parsed.Limit, parsed.Window, parsed.TTL, cost)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported algorithm")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	latency := time.Since(start)
	allowedStr := "false"
	if res.Allowed {
		allowedStr = "true"
	}
	s.reqs.WithLabelValues(allowedStr, string(algo)).Inc()
	s.lat.WithLabelValues(string(algo)).Observe(latency.Seconds())

	// Structured JSON log — Promtail ships these lines to Loki.
	log.Info().
		Str("ns", req.GetNamespace()).
		Str("key", key).
		Str("algo", string(algo)).
		Bool("allowed", res.Allowed).
		Int64("remaining", res.Remaining).
		Int64("latency_us", latency.Microseconds()).
		Msg("allow")

	// Async publish to Redis Streams — fire-and-forget.
	s.pub.publish(StreamEvent{
		Timestamp:  time.Now().UnixMilli(),
		Namespace:  req.GetNamespace(),
		Key:        key,
		Algo:       string(algo),
		Allowed:    res.Allowed,
		Remaining:  res.Remaining,
		RetryMS:    res.RetryAfterMS,
		LatencyUS:  latency.Microseconds(),
		InstanceID: s.instanceID,
	})

	return &ratelimitv1.AllowResponse{
		Allowed:       res.Allowed,
		Remaining:     res.Remaining,
		RetryAfterMs:  res.RetryAfterMS,
		AlgorithmUsed: string(algo),
	}, nil
}

func main() {
	// Structured JSON logging — every line is parseable by Promtail/Loki.
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	hostname, _ := os.Hostname()
	log.Logger = zerolog.New(os.Stdout).With().
		Str("service", "ratelimiter").
		Str("instance", hostname).
		Timestamp().
		Logger()

	grpcAddr := env("GRPC_ADDR", "0.0.0.0:50051")
	metricsAddr := env("METRICS_ADDR", "0.0.0.0:2112")
	redisAddrs := env("REDIS_ADDRS", "redis-1:7001,redis-2:7002,redis-3:7003,redis-4:7004,redis-5:7005,redis-6:7006")

	// OTel tracing (no-op if OTEL_EXPORTER_OTLP_ENDPOINT unset).
	ctx := context.Background()
	shutdown, err := initTracer(ctx, "ratelimiter", hostname)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init tracer")
	}
	defer shutdown(ctx) //nolint:errcheck

	rdb := limiter.NewRedisCluster(redisAddrs)
	defer rdb.Close()

	pub := newStreamPublisher(rdb)
	defer pub.stop()

	// Metrics HTTP server.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		log.Info().Str("addr", metricsAddr).Msg("metrics listening")
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			log.Fatal().Err(err).Msg("metrics server failed")
		}
	}()

	// gRPC server with OTel stats handler for distributed tracing.
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("listen failed")
	}
	gs := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)
	ratelimitv1.RegisterRateLimitServiceServer(gs, newSrv(limiter.New(rdb), pub, hostname))
	reflection.Register(gs)

	log.Info().Str("addr", grpcAddr).Msg("gRPC listening")
	if err := gs.Serve(lis); err != nil {
		log.Fatal().Err(err).Msg("gRPC server failed")
	}
}

func env(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
