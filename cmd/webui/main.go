package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed index.html
var indexHTML string

type allowReq struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Rule      string `json:"rule"`
	Algorithm string `json:"algorithm"` // "", "TOKEN_BUCKET", or "SLIDING_WINDOW"
	Cost      int64  `json:"cost"`
}

type observabilityResp struct {
	GeneratedAt string                  `json:"generated_at"`
	Stats       map[string]float64      `json:"stats"`
	Series      map[string][]chartPoint `json:"series"`
}

type chartPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

type promAPIResponse struct {
	Status string      `json:"status"`
	Data   promAPIData `json:"data"`
	Error  string      `json:"error"`
}

type promAPIData struct {
	ResultType string            `json:"resultType"`
	Result     []json.RawMessage `json:"result"`
}

type promVectorResult struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

type promMatrixResult struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

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

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	hostname, _ := os.Hostname()
	log.Logger = zerolog.New(os.Stdout).With().
		Str("service", "webui").
		Str("instance", hostname).
		Timestamp().
		Logger()

	httpAddr := env("HTTP_ADDR", "0.0.0.0:8080")
	grpcTarget := env("GRPC_TARGET", "envoy:50051")
	promURL := env("PROMETHEUS_URL", "http://prometheus:9090")
	debugDashURL := env("DEBUG_DASHBOARD_URL", "http://debug-dashboard:4000")
	streamerURL := env("STREAMER_URL", "http://streamer:8888")

	ctx := context.Background()
	shutdown, err := initTracer(ctx, "webui", hostname)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init tracer")
	}
	defer shutdown(ctx) //nolint:errcheck

	// gRPC client with OTel stats handler propagates trace context to the rate limiter.
	conn, err := grpc.Dial(grpcTarget,
		grpc.WithInsecure(),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("grpc dial failed")
	}
	defer conn.Close()

	client := ratelimitv1.NewRateLimitServiceClient(conn)

	// Wrap mux with OTel HTTP instrumentation so every request gets a trace span.
	handler := otelhttp.NewHandler(newMux(client, promURL, debugDashURL, streamerURL), "webui")

	log.Info().Str("addr", httpAddr).Str("grpc_target", grpcTarget).Msg("webui listening")
	if err := http.ListenAndServe(httpAddr, handler); err != nil {
		log.Fatal().Err(err).Msg("http server failed")
	}
}

func newMux(client ratelimitv1.RateLimitServiceClient, promURL, debugDashURL, streamerURL string) *http.ServeMux {
	mux := http.NewServeMux()
	promClient := &http.Client{Timeout: 3 * time.Second}
	proxyClient := &http.Client{Timeout: 5 * time.Second}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(indexHTML))
	})

	mux.HandleFunc("/api/allow", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var ar allowReq
		if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if ar.Cost <= 0 {
			ar.Cost = 1
		}

		algo := ratelimitv1.Algorithm_ALGO_UNSPECIFIED
		switch strings.ToUpper(strings.TrimSpace(ar.Algorithm)) {
		case "", "AUTO":
		case "TOKEN_BUCKET":
			algo = ratelimitv1.Algorithm_TOKEN_BUCKET
		case "SLIDING_WINDOW":
			algo = ratelimitv1.Algorithm_SLIDING_WINDOW
		default:
			http.Error(w, "unsupported algorithm", http.StatusBadRequest)
			return
		}

		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()

		resp, err := client.Allow(ctx, &ratelimitv1.AllowRequest{
			Namespace: ar.Namespace,
			Key:       ar.Key,
			Rule:      ar.Rule,
			Algorithm: algo,
			Cost:      ar.Cost,
		})

		log.Info().
			Str("ns", ar.Namespace).
			Str("key", ar.Key).
			Str("algo", ar.Algorithm).
			Int64("latency_us", time.Since(start).Microseconds()).
			Bool("error", err != nil).
			Msg("api/allow")

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(httpStatusFromGRPCError(err))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/observability", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}

		payload, err := loadObservability(r.Context(), promClient, promURL)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Proxy routes — forward to debug-dashboard and streamer via internal Docker DNS.
	mux.HandleFunc("/api/service-health", proxyJSON(proxyClient, debugDashURL+"/api/health"))
	mux.HandleFunc("/api/active-alerts", proxyJSON(proxyClient, debugDashURL+"/api/alerts"))
	mux.HandleFunc("/api/recent-logs", proxyJSON(proxyClient, debugDashURL+"/api/logs"))
	mux.HandleFunc("/api/chaos/status", proxyJSON(proxyClient, debugDashURL+"/api/chaos/status"))
	mux.HandleFunc("/api/chaos/kill-ratelimiter", proxyJSON(proxyClient, debugDashURL+"/api/chaos/kill-ratelimiter"))
	mux.HandleFunc("/api/chaos/kill-redis", proxyJSON(proxyClient, debugDashURL+"/api/chaos/kill-redis"))
	mux.HandleFunc("/api/chaos/restore", proxyJSON(proxyClient, debugDashURL+"/api/chaos/restore"))
	mux.HandleFunc("/api/stream-stats", proxyJSON(proxyClient, streamerURL+"/api/stats"))

	return mux
}

// proxyJSON forwards a request to targetURL and streams the JSON response back.
// Query string and request body are forwarded as-is.
func proxyJSON(httpClient *http.Client, targetURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		u := targetURL
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(ctx, r.Method, u, r.Body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		defer resp.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func loadObservability(ctx context.Context, client *http.Client, promURL string) (observabilityResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	now := time.Now().UTC()
	start := now.Add(-10 * time.Minute)
	step := 5 * time.Second

	statsQueries := map[string]string{
		"request_rate":           "sum(rate(ratelimiter_requests_total[1m]))",
		"allowed_rate":           `sum(rate(ratelimiter_requests_total{allowed="true"}[1m]))`,
		"denied_rate":            `sum(rate(ratelimiter_requests_total{allowed="false"}[1m]))`,
		"latency_p95_ms":         "histogram_quantile(0.95, sum by (le) (rate(ratelimiter_allow_latency_seconds_bucket[5m]))) * 1000",
		"ratelimiter_targets_up": "sum(up{job=\"ratelimiter\"})",
	}

	stats := make(map[string]float64, len(statsQueries))
	for key, query := range statsQueries {
		value, err := promQueryInstant(ctx, client, promURL, query)
		if err != nil {
			return observabilityResp{}, err
		}
		stats[key] = value
	}

	seriesQueries := map[string]string{
		"request_rate":   "sum(rate(ratelimiter_requests_total[1m]))",
		"allowed_rate":   `sum(rate(ratelimiter_requests_total{allowed="true"}[1m]))`,
		"denied_rate":    `sum(rate(ratelimiter_requests_total{allowed="false"}[1m]))`,
		"latency_p95_ms": "histogram_quantile(0.95, sum by (le) (rate(ratelimiter_allow_latency_seconds_bucket[5m]))) * 1000",
	}

	series := make(map[string][]chartPoint, len(seriesQueries))
	for key, query := range seriesQueries {
		points, err := promQueryRange(ctx, client, promURL, query, start, now, step)
		if err != nil {
			return observabilityResp{}, err
		}
		series[key] = points
	}

	return observabilityResp{
		GeneratedAt: now.Format(time.RFC3339),
		Stats:       stats,
		Series:      series,
	}, nil
}

func promQueryInstant(ctx context.Context, client *http.Client, baseURL, query string) (float64, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/v1/query")
	if err != nil {
		return 0, err
	}

	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	var res promAPIResponse
	if err := promDo(ctx, client, u.String(), &res); err != nil {
		return 0, err
	}
	if res.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", res.Error)
	}
	if len(res.Data.Result) == 0 {
		return 0, nil
	}

	var vector promVectorResult
	if err := json.Unmarshal(res.Data.Result[0], &vector); err != nil {
		return 0, err
	}
	return parsePromSample(vector.Value)
}

func promQueryRange(ctx context.Context, client *http.Client, baseURL, query string, start, end time.Time, step time.Duration) ([]chartPoint, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))
	u.RawQuery = q.Encode()

	var res promAPIResponse
	if err := promDo(ctx, client, u.String(), &res); err != nil {
		return nil, err
	}
	if res.Status != "success" {
		return nil, fmt.Errorf("prometheus range query failed: %s", res.Error)
	}
	if len(res.Data.Result) == 0 {
		return []chartPoint{}, nil
	}

	var matrix promMatrixResult
	if err := json.Unmarshal(res.Data.Result[0], &matrix); err != nil {
		return nil, err
	}

	points := make([]chartPoint, 0, len(matrix.Values))
	for _, sample := range matrix.Values {
		value, err := parsePromSample(sample)
		if err != nil {
			return nil, err
		}
		ts, err := parsePromTimestamp(sample[0])
		if err != nil {
			return nil, err
		}
		points = append(points, chartPoint{Timestamp: ts, Value: value})
	}

	return points, nil
}

func promDo(ctx context.Context, client *http.Client, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prometheus returned %s", resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

func parsePromSample(sample []any) (float64, error) {
	if len(sample) < 2 {
		return 0, fmt.Errorf("invalid prometheus sample")
	}
	text, ok := sample[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid prometheus sample value")
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, err
	}
	return sanitizeFloat(value), nil
}

func parsePromTimestamp(raw any) (int64, error) {
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, err
		}
		return int64(f), nil
	default:
		return 0, fmt.Errorf("invalid prometheus timestamp")
	}
}

func sanitizeFloat(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func httpStatusFromGRPCError(err error) int {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusBadGateway
	}

	switch st.Code() {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Unavailable:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

func env(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
