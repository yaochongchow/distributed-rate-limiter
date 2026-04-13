package main

import (
	"context"
	"testing"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestSrv() *srv {
	return &srv{
		reqs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_requests_total",
			Help: "test",
		}, []string{"allowed", "algo"}),
		lat: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "test_allow_latency_seconds",
			Help: "test",
		}, []string{"algo"}),
	}
}

func TestAllowRejectsMismatchedAlgorithm(t *testing.T) {
	s := newTestSrv()

	_, err := s.Allow(context.Background(), &ratelimitv1.AllowRequest{
		Key:       "user1",
		Rule:      "10rps",
		Algorithm: ratelimitv1.Algorithm_SLIDING_WINDOW,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestAllowRejectsSlidingWindowCostAboveLimit(t *testing.T) {
	s := newTestSrv()

	_, err := s.Allow(context.Background(), &ratelimitv1.AllowRequest{
		Key:  "user1",
		Rule: "2/1s",
		Cost: 3,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestAllowRejectsTokenBucketCostAboveCapacity(t *testing.T) {
	s := newTestSrv()

	_, err := s.Allow(context.Background(), &ratelimitv1.AllowRequest{
		Key:  "user1",
		Rule: "2rps",
		Cost: 5,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want %v", status.Code(err), codes.InvalidArgument)
	}
}
