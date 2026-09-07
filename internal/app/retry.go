package app

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// retryStop wraps an error that must end a retry loop rather than be retried.
// It lives beside retryUntil because that is the only thing that acts on it;
// a caller that wraps without the loop honouring it gets silence.
type retryStop struct{ err error }

func (r retryStop) Error() string { return r.err.Error() }

func (r retryStop) Unwrap() error { return r.err }

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
		// Some failures cannot be waited out -- a node on the wrong chain stays
		// on the wrong chain. fn says so by wrapping in retryStop, and the
		// caller unwraps to decide what to do about it. Without this branch the
		// wrapper is decoration: the loop below treats it as any other error
		// and retries an error the caller declared fatal, forever.
		var stop retryStop
		if errors.As(err, &stop) {
			return err
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
