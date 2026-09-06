// Package ratelimit is the token-bucket limiter of docs/plan-v1.0.md §14:
// Redis-backed when a client is available (shared across api replicas),
// in-memory otherwise (single replica, degraded but never a source of
// truth). Limits are "N per window" with a burst equal to N.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limit is N events per Window (burst N).
type Limit struct {
	N      int64
	Window time.Duration
}

// ParseLimit reads "20/1s", "5/1m", "100/1h".
func ParseLimit(s string) (Limit, error) {
	n, w, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Limit{}, fmt.Errorf("ratelimit: %q must be N/duration", s)
	}
	count, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	if err != nil || count <= 0 {
		return Limit{}, fmt.Errorf("ratelimit: %q: count must be a positive integer", s)
	}
	window, err := time.ParseDuration(strings.TrimSpace(w))
	if err != nil || window <= 0 {
		return Limit{}, fmt.Errorf("ratelimit: %q: bad window", s)
	}
	return Limit{N: count, Window: window}, nil
}

// String renders the limit as N/window.
func (l Limit) String() string { return fmt.Sprintf("%d/%s", l.N, l.Window) }

// interval is the refill period of one token.
func (l Limit) interval() time.Duration { return l.Window / time.Duration(l.N) }

// Decision is the outcome of Allow.
type Decision struct {
	Allowed    bool
	Remaining  int64
	RetryAfter time.Duration // > 0 only when denied
}

// Limiter decides whether an event under key fits the limit.
type Limiter interface {
	Allow(ctx context.Context, key string, l Limit) (Decision, error)
}

// --- in-memory ---

type bucket struct {
	tokens int64
	last   time.Time // time the bucket was last refilled to `tokens`
}

// Memory is a per-process limiter. Keys that have been idle for longer than
// their window are dropped on the next sweep.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	sweeps  int
}

// NewMemory creates an in-memory limiter.
func NewMemory() *Memory {
	return &Memory{buckets: map[string]*bucket{}, now: time.Now}
}

// WithClock overrides the clock (tests).
func (m *Memory) WithClock(now func() time.Time) *Memory {
	m.now = now
	return m
}

// Allow implements Limiter with integer token-bucket arithmetic: one token
// refills every Window/N; a full bucket holds N.
func (m *Memory) Allow(_ context.Context, key string, l Limit) (Decision, error) {
	if l.N <= 0 || l.Window <= 0 {
		return Decision{}, errors.New("ratelimit: invalid limit")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
	if m.sweeps%1024 == 0 {
		for k, b := range m.buckets {
			if now.Sub(b.last) > 2*l.Window {
				delete(m.buckets, k)
			}
		}
	}
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{tokens: l.N, last: now}
		m.buckets[key] = b
	}
	refill(b, l, now)
	if b.tokens > 0 {
		b.tokens--
		return Decision{Allowed: true, Remaining: b.tokens}, nil
	}
	return Decision{Allowed: false, Remaining: 0, RetryAfter: l.interval() - now.Sub(b.last)}, nil
}

func refill(b *bucket, l Limit, now time.Time) {
	iv := l.interval()
	if elapsed := now.Sub(b.last); elapsed >= iv {
		n := int64(elapsed / iv)
		b.tokens = min(l.N, b.tokens+n)
		if b.tokens == l.N {
			b.last = now
		} else {
			b.last = b.last.Add(time.Duration(n) * iv)
		}
	}
}

// --- redis ---

// Same algorithm in Lua so every api replica shares one bucket per key.
// KEYS[1] bucket hash; ARGV: capacity, interval_ms, now_ms.
// Returns {allowed(0|1), remaining, retry_after_ms}.
const luaBucketScript = `
local cap = tonumber(ARGV[1])
local iv = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local tokens = tonumber(redis.call('HGET', KEYS[1], 't'))
local last = tonumber(redis.call('HGET', KEYS[1], 'l'))
if tokens == nil then tokens = cap; last = now end
local elapsed = now - last
if elapsed >= iv then
  local n = math.floor(elapsed / iv)
  tokens = math.min(cap, tokens + n)
  if tokens == cap then last = now else last = last + n * iv end
end
local allowed = 0
local retry = 0
if tokens > 0 then
  tokens = tokens - 1
  allowed = 1
else
  retry = iv - (now - last)
end
redis.call('HSET', KEYS[1], 't', tokens, 'l', last)
redis.call('PEXPIRE', KEYS[1], iv * cap * 2)
return {allowed, tokens, retry}
`

// Redis is the shared limiter.
type Redis struct {
	rdb    *redis.Client
	script *redis.Script
	prefix string
	now    func() time.Time
}

// NewRedis wraps a client; keys are prefixed with "rl:".
func NewRedis(rdb *redis.Client) *Redis {
	return &Redis{rdb: rdb, script: redis.NewScript(luaBucketScript), prefix: "rl:", now: time.Now}
}

// Allow implements Limiter. Redis errors are returned so the caller can
// fall back (Fallback does this).
func (r *Redis) Allow(ctx context.Context, key string, l Limit) (Decision, error) {
	if l.N <= 0 || l.Window <= 0 {
		return Decision{}, errors.New("ratelimit: invalid limit")
	}
	res, err := r.script.Run(ctx, r.rdb, []string{r.prefix + key}, l.N, l.interval().Milliseconds(), r.now().UnixMilli()).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: redis: %w", err)
	}
	if len(res) != 3 {
		return Decision{}, errors.New("ratelimit: redis: unexpected reply")
	}
	return Decision{Allowed: res[0] == 1, Remaining: res[1], RetryAfter: time.Duration(res[2]) * time.Millisecond}, nil
}

// Fallback uses primary and switches to secondary for a call when primary
// fails (Redis down): limits keep working per replica instead of failing
// open or closed.
type Fallback struct {
	Primary   Limiter
	Secondary Limiter
	OnError   func(error)
}

// Allow implements Limiter.
func (f Fallback) Allow(ctx context.Context, key string, l Limit) (Decision, error) {
	d, err := f.Primary.Allow(ctx, key, l)
	if err == nil {
		return d, nil
	}
	if f.OnError != nil {
		f.OnError(err)
	}
	return f.Secondary.Allow(ctx, key, l)
}
