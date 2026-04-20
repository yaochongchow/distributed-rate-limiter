package limiter

import (
	"context"
	_ "embed"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed token_bucket.lua
var tokenBucketLua string

//go:embed sliding_window.lua
var slidingWindowLua string

type Result struct {
	Allowed      bool
	Remaining    int64
	RetryAfterMS int64
}

type Limiter struct {
	rdb *redis.ClusterClient
	tb  *redis.Script
	sw  *redis.Script
}

func New(rdb *redis.ClusterClient) *Limiter {
	return &Limiter{
		rdb: rdb,
		tb:  redis.NewScript(tokenBucketLua),
		sw:  redis.NewScript(slidingWindowLua),
	}
}

func (l *Limiter) TokenBucket(ctx context.Context, key string, capacity int64, refillPerSec float64, ttl time.Duration, cost int64) (Result, error) {
	now := time.Now()
	nowMs := now.UnixMilli()
	ttlMs := int64(ttl.Milliseconds())

	vals, err := l.tb.Run(ctx, l.rdb, []string{key},
		capacity,
		refillPerSec,
		nowMs,
		ttlMs,
		cost,
	).Result()
	if err != nil {
		ReloadClusterStateOnError(l.rdb, err)
		return Result{}, err
	}

	arr := vals.([]interface{})
	allowed := arr[0].(int64) == 1
	remaining := arr[1].(int64)
	retryAfterMs := arr[2].(int64)

	return Result{
		Allowed:      allowed,
		Remaining:    remaining,
		RetryAfterMS: retryAfterMs,
	}, nil
}

func (l *Limiter) SlidingWindow(ctx context.Context, key string, limit int64, window time.Duration, ttl time.Duration, cost int64) (Result, error) {
	now := time.Now()
	nowMs := now.UnixMilli()
	windowMs := int64(window.Milliseconds())
	ttlMs := int64(ttl.Milliseconds())

	vals, err := l.sw.Run(ctx, l.rdb, []string{key},
		limit,
		windowMs,
		nowMs,
		ttlMs,
		cost,
	).Result()
	if err != nil {
		ReloadClusterStateOnError(l.rdb, err)
		return Result{}, err
	}

	arr := vals.([]interface{})
	allowed := arr[0].(int64) == 1
	remaining := arr[1].(int64)
	retryAfterMs := arr[2].(int64)

	return Result{
		Allowed:      allowed,
		Remaining:    remaining,
		RetryAfterMS: retryAfterMs,
	}, nil
}
