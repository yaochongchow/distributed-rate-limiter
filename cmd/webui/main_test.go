package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRateLimitClient struct {
	allow func(context.Context, *ratelimitv1.AllowRequest, ...grpc.CallOption) (*ratelimitv1.AllowResponse, error)
}

func (f fakeRateLimitClient) Allow(ctx context.Context, req *ratelimitv1.AllowRequest, opts ...grpc.CallOption) (*ratelimitv1.AllowResponse, error) {
	return f.allow(ctx, req, opts...)
}

func TestAllowHandlerMapsInvalidArgumentToBadRequest(t *testing.T) {
	mux := newMux(fakeRateLimitClient{
		allow: func(context.Context, *ratelimitv1.AllowRequest, ...grpc.CallOption) (*ratelimitv1.AllowResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "bad request")
		},
	}, "http://prometheus:9090", "http://debug-dashboard:4000", "http://streamer:8888")

	body, err := json.Marshal(allowReq{
		Namespace: "api",
		Key:       "user1",
		Rule:      "10rps",
		Algorithm: "AUTO",
		Cost:      1,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/allow", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAllowHandlerMapsDeadlineExceededToGatewayTimeout(t *testing.T) {
	mux := newMux(fakeRateLimitClient{
		allow: func(context.Context, *ratelimitv1.AllowRequest, ...grpc.CallOption) (*ratelimitv1.AllowResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "timeout")
		},
	}, "http://prometheus:9090", "http://debug-dashboard:4000", "http://streamer:8888")

	body, err := json.Marshal(allowReq{
		Namespace: "api",
		Key:       "user1",
		Rule:      "10rps",
		Algorithm: "AUTO",
		Cost:      1,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/allow", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusGatewayTimeout)
	}
}

func TestAllowHandlerEmitsExplicitDeniedFields(t *testing.T) {
	mux := newMux(fakeRateLimitClient{
		allow: func(context.Context, *ratelimitv1.AllowRequest, ...grpc.CallOption) (*ratelimitv1.AllowResponse, error) {
			return &ratelimitv1.AllowResponse{
				Allowed:       false,
				Remaining:     0,
				RetryAfterMs:  1234,
				AlgorithmUsed: "sliding_window",
			}, nil
		},
	}, "http://prometheus:9090", "http://debug-dashboard:4000", "http://streamer:8888")

	body, err := json.Marshal(allowReq{
		Namespace: "auth",
		Key:       "user123",
		Rule:      "5/1m",
		Algorithm: "AUTO",
		Cost:      1,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/allow", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusOK)
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if allowed, ok := got["allowed"].(bool); !ok || allowed {
		t.Fatalf("expected explicit allowed=false, got %#v", got["allowed"])
	}
	if remaining, ok := got["remaining"].(float64); !ok || remaining != 0 {
		t.Fatalf("expected explicit remaining=0, got %#v", got["remaining"])
	}
	if retryAfter, ok := got["retry_after_ms"].(float64); !ok || retryAfter != 1234 {
		t.Fatalf("expected retry_after_ms=1234, got %#v", got["retry_after_ms"])
	}
}
