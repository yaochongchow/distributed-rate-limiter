package limiter

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

func NewRedisCluster(addrsCSV string) *redis.ClusterClient {
	var addrs []string
	for _, a := range strings.Split(addrsCSV, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			addrs = append(addrs, a)
		}
	}
	return redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:          addrs,
		PoolSize:       300,
		MinIdleConns:   50,
		MaxRetries:     2,
		RouteByLatency: true,
		RouteRandomly:  true,
	})
}

// ReloadClusterStateOnError forces go-redis to refresh cluster topology when
// a request fails with a stale-node or cluster-recovery style error.
func ReloadClusterStateOnError(rdb *redis.ClusterClient, err error) bool {
	if rdb == nil || err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "clusterdown") &&
		!strings.Contains(msg, "connection refused") &&
		!strings.Contains(msg, "no route to host") &&
		!strings.Contains(msg, "i/o timeout") &&
		!strings.Contains(msg, "network is unreachable") &&
		!strings.Contains(msg, "client is closed") &&
		!strings.Contains(msg, "use of closed network connection") {
		return false
	}

	rdb.ReloadState(context.Background())
	return true
}
