package limiter

import (
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
