// Package pg wraps pgx connection pooling, health checks and the
// NUMERIC ↔ money.Amount conversion used by every persistence layer.
package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig configures Open.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	ConnectTimeout  time.Duration
	ApplicationName string
	// Tracing attaches the query tracer (one client span per statement
	// under a sampled span). Off, pgx never calls into it.
	Tracing bool
}

// Open parses the DSN, applies the pool settings and pings once. Retrying on
// startup is the caller's job (internal/app does exponential backoff).
func Open(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pg: parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.ConnectTimeout > 0 {
		pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}
	if cfg.ApplicationName != "" {
		if pc.ConnConfig.RuntimeParams == nil {
			pc.ConnConfig.RuntimeParams = map[string]string{}
		}
		pc.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}
	if cfg.Tracing {
		pc.ConnConfig.Tracer = queryTracer{}
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("pg: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return pool, nil
}

// HealthCheck returns a readiness probe for the pool.
func HealthCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error { return pool.Ping(ctx) }
}
