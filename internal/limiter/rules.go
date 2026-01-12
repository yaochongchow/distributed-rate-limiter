package limiter

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Algo string

const (
	AlgoTB Algo = "token_bucket"
	AlgoSW Algo = "sliding_window"
)

type ParsedRule struct {
	Algo         Algo
	Limit        int64
	Window       time.Duration
	RefillPerSec float64
	Capacity     int64
	TTL          time.Duration
}

func ParseRule(rule string) (ParsedRule, error) {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return ParsedRule{}, fmt.Errorf("empty rule")
	}

	// Sliding window: "1000/1s"
	if strings.Contains(rule, "/") {
		parts := strings.Split(rule, "/")
		if len(parts) != 2 {
			return ParsedRule{}, fmt.Errorf("bad sliding window rule: %q", rule)
		}
		limit, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || limit <= 0 {
			return ParsedRule{}, fmt.Errorf("bad limit: %q", parts[0])
		}
		win, err := time.ParseDuration(parts[1])
		if err != nil || win <= 0 {
			return ParsedRule{}, fmt.Errorf("bad window: %q", parts[1])
		}
		return ParsedRule{
			Algo:   AlgoSW,
			Limit:  limit,
			Window: win,
			TTL:    2 * win, // TTL-based cleanup
		}, nil
	}

	// Token bucket: "1000rps"
	if strings.HasSuffix(rule, "rps") {
		n := strings.TrimSuffix(rule, "rps")
		limit, err := strconv.ParseInt(n, 10, 64)
		if err != nil || limit <= 0 {
			return ParsedRule{}, fmt.Errorf("bad rps: %q", rule)
		}
		return ParsedRule{
			Algo:         AlgoTB,
			Limit:        limit,
			RefillPerSec: float64(limit),
			Capacity:     limit * 2, // default burst=2s
			TTL:          5 * time.Second,
		}, nil
	}

	return ParsedRule{}, fmt.Errorf("unknown rule format: %q (use 1000rps or 1000/1s)", rule)
}
