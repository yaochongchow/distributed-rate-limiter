package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"
	"example.com/distributed-rate-limiter/internal/limiter"

	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type srv struct {
	ratelimitv1.UnimplementedRateLimitServiceServer
	lim *limiter.Limiter

	reqs *prometheus.CounterVec
	lat  *prometheus.HistogramVec
}

func newSrv(l *limiter.Limiter) *srv {
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
	return &srv{lim: l, reqs: reqs, lat: lat}
}

// Redis Cluster key: use hash tags so one client key stays on one slot.
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

	allowedStr := "false"
	if res.Allowed {
		allowedStr = "true"
	}
	s.reqs.WithLabelValues(allowedStr, string(algo)).Inc()
	s.lat.WithLabelValues(string(algo)).Observe(time.Since(start).Seconds())

	return &ratelimitv1.AllowResponse{
		Allowed:       res.Allowed,
		Remaining:     res.Remaining,
		RetryAfterMs:  res.RetryAfterMS,
		AlgorithmUsed: string(algo),
	}, nil
}

func main() {
	grpcAddr := env("GRPC_ADDR", "0.0.0.0:50051")
	metricsAddr := env("METRICS_ADDR", "0.0.0.0:2112")
	redisAddrs := env("REDIS_ADDRS", "redis-1:7001,redis-2:7002,redis-3:7003,redis-4:7004,redis-5:7005,redis-6:7006")

	rdb := limiter.NewRedisCluster(redisAddrs)
	defer rdb.Close()

	// Metrics HTTP
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		log.Printf("metrics listening on %s", metricsAddr)
		log.Fatal(http.ListenAndServe(metricsAddr, mux))
	}()

	// gRPC
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}
	gs := grpc.NewServer()
	ratelimitv1.RegisterRateLimitServiceServer(gs, newSrv(limiter.New(rdb)))
	reflection.Register(gs)

	log.Printf("gRPC listening on %s", grpcAddr)
	log.Fatal(gs.Serve(lis))
}

func env(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
