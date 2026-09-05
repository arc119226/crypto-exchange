package app

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

var (
	retryBase = 200 * time.Millisecond
	retryMax  = 30 * time.Second
	// retrySleep is overridable in tests.
	retrySleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// retryUntil calls fn with exponential backoff (base × 2^n plus up to 25 %
// jitter, capped at retryMax) until it succeeds or ctx is cancelled.
// Dependencies are never assumed to be ready; compose depends_on is only an
// accelerator (docs/plan-v1.0.md §11).
func retryUntil(ctx context.Context, log *slog.Logger, name string, fn func(context.Context) error) error {
	backoff := retryBase
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		jitter := time.Duration(rand.Int64N(int64(backoff/4) + 1)) //nolint:gosec // jitter only, not security sensitive
		wait := backoff + jitter
		log.Warn("dependency not ready, retrying",
			slog.String("dependency", name),
			slog.Int("attempt", attempt),
			slog.Duration("next_backoff", wait),
			slog.String("err", err.Error()))
		if err := retrySleep(ctx, wait); err != nil {
			return err
		}
		backoff = min(backoff*2, retryMax)
	}
}
