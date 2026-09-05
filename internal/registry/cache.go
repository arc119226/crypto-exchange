package registry

import (
	"context"
	"sync"
)

// Cache is an in-memory snapshot of one tenant's registry, loaded from a
// Reader at start-up. The engine consults it on the hot path; `reload`
// (Phase 3) swaps in a fresh snapshot.
type Cache struct {
	mu      sync.RWMutex
	tenant  string
	assets  map[string]Asset
	markets map[string]Market
}

// NewCache creates an empty cache for the tenant.
func NewCache(tenantID string) *Cache {
	return &Cache{tenant: tenantID, assets: map[string]Asset{}, markets: map[string]Market{}}
}

// Load replaces the snapshot atomically.
func (c *Cache) Load(ctx context.Context, r Reader) error {
	assets, err := r.ListAssets(ctx, c.tenant)
	if err != nil {
		return err
	}
	markets, err := r.ListMarkets(ctx, c.tenant)
	if err != nil {
		return err
	}
	am := make(map[string]Asset, len(assets))
	for _, a := range assets {
		am[a.Symbol] = a
	}
	mm := make(map[string]Market, len(markets))
	for _, m := range markets {
		mm[m.Symbol] = m
	}
	c.mu.Lock()
	c.assets, c.markets = am, mm
	c.mu.Unlock()
	return nil
}

// Asset looks up an asset by symbol.
func (c *Cache) Asset(symbol string) (Asset, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.assets[symbol]
	return a, ok
}

// Market looks up a market by symbol.
func (c *Cache) Market(symbol string) (Market, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.markets[symbol]
	return m, ok
}

// Markets returns all markets (unordered copy).
func (c *Cache) Markets() []Market {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Market, 0, len(c.markets))
	for _, m := range c.markets {
		out = append(out, m)
	}
	return out
}
