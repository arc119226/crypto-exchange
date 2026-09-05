// Package redisx wraps the optional Redis client (rate-limit counters and
// depth snapshot cache only; never a source of truth).
package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open creates a client; no network call happens until first use.
func Open(addr, password string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DialTimeout: 3 * time.Second,
		ReadTimeout: 2 * time.Second,
	})
}

// HealthCheck returns a readiness probe for the client.
func HealthCheck(rdb *redis.Client) func(context.Context) error {
	return func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
}
