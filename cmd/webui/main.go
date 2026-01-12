package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	ratelimitv1 "example.com/distributed-rate-limiter/gen"

	"google.golang.org/grpc"
)

//go:embed index.html
var indexHTML string

type allowReq struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Rule      string `json:"rule"`
	Algorithm string `json:"algorithm"` // "TOKEN_BUCKET" or "SLIDING_WINDOW"
	Cost      int64  `json:"cost"`
}

func main() {
	httpAddr := env("HTTP_ADDR", "0.0.0.0:8080")
	grpcTarget := env("GRPC_TARGET", "envoy:50051") // internal docker DNS

	conn, err := grpc.Dial(grpcTarget, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("grpc dial error: %v", err)
	}
	defer conn.Close()

	client := ratelimitv1.NewRateLimitServiceClient(conn)

	mux := http.NewServeMux()

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
		case "TOKEN_BUCKET":
			algo = ratelimitv1.Algorithm_TOKEN_BUCKET
		case "SLIDING_WINDOW":
			algo = ratelimitv1.Algorithm_SLIDING_WINDOW
		}

		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()

		resp, err := client.Allow(ctx, &ratelimitv1.AllowRequest{
			Namespace: ar.Namespace,
			Key:       ar.Key,
			Rule:      ar.Rule,
			Algorithm: algo,
			Cost:      ar.Cost,
		})
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	log.Printf("webui listening on %s (grpc target %s)", httpAddr, grpcTarget)
	log.Fatal(http.ListenAndServe(httpAddr, mux))
}

func env(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
