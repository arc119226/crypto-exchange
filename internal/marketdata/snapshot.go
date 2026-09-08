package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// DepthCache is where the api role reads a depth snapshot without asking
// the engine (docs/plan-v1.0.md §12 "Redis 深度快照快取"). ok is false when
// there is no snapshot for the market.
type DepthCache interface {
	GetDepth(ctx context.Context, market string) (d Depth, at time.Time, ok bool, err error)
}

// SnapshotCache keeps the latest depth of every market in Redis. The stream
// role writes it (it already holds the book), the api role reads it.
type SnapshotCache struct {
	rdb    *redis.Client
	tenant string
	ttl    time.Duration
}

// NewSnapshotCache wraps a client; entries expire after ttl.
func NewSnapshotCache(rdb *redis.Client, tenant string, ttl time.Duration) *SnapshotCache {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &SnapshotCache{rdb: rdb, tenant: tenant, ttl: ttl}
}

var _ DepthCache = (*SnapshotCache)(nil)

// cachedDepth is the stored form: the depth plus when it was taken, so a
// reader can tell a fresh snapshot from one the stream stopped refreshing.
type cachedDepth struct {
	At    time.Time `json:"at"`
	Depth Depth     `json:"depth"`
}

func (c *SnapshotCache) key(market string) string { return "md:depth:" + c.tenant + ":" + market }

// PutDepth stores a snapshot taken at at.
func (c *SnapshotCache) PutDepth(ctx context.Context, d Depth, at time.Time) error {
	b, err := json.Marshal(cachedDepth{At: at.UTC(), Depth: d})
	if err != nil {
		return fmt.Errorf("marketdata: encode depth: %w", err)
	}
	if err := c.rdb.Set(ctx, c.key(d.Market), b, c.ttl).Err(); err != nil {
		return fmt.Errorf("marketdata: cache depth: %w", err)
	}
	return nil
}

// GetDepth implements DepthCache.
func (c *SnapshotCache) GetDepth(ctx context.Context, market string) (Depth, time.Time, bool, error) {
	b, err := c.rdb.Get(ctx, c.key(market)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Depth{}, time.Time{}, false, nil
	}
	if err != nil {
		return Depth{}, time.Time{}, false, fmt.Errorf("marketdata: read depth cache: %w", err)
	}
	var cd cachedDepth
	if err := json.Unmarshal(b, &cd); err != nil {
		return Depth{}, time.Time{}, false, fmt.Errorf("marketdata: decode depth cache: %w", err)
	}
	return cd.Depth, cd.At, true, nil
}
