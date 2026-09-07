package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	orig := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { retrySleep = orig })
	return &waits
}

func TestRetryUntilSucceedsAfterFailures(t *testing.T) {
	waits := stubSleep(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	err := retryUntil(context.Background(), log, "dep", func(context.Context) error {
		calls++
		if calls < 4 {
			return errors.New("not yet")
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, calls)
	require.Len(t, *waits, 3)
	// exponential: base, 2×base, 4×base, each plus ≤ 25 % jitter
	for i, w := range *waits {
		lo := retryBase << i
		assert.GreaterOrEqual(t, w, lo, "wait %d", i)
		assert.LessOrEqual(t, w, lo+lo/4+time.Nanosecond, "wait %d", i)
	}
}

func TestRetryUntilCapsBackoff(t *testing.T) {
	waits := stubSleep(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	_ = retryUntil(context.Background(), log, "dep", func(context.Context) error {
		calls++
		if calls <= 12 {
			return errors.New("down")
		}
		return nil
	})
	last := (*waits)[len(*waits)-1]
	assert.LessOrEqual(t, last, retryMax+retryMax/4)
	assert.GreaterOrEqual(t, last, retryMax)
}

func TestRetryUntilStopsOnCancel(t *testing.T) {
	stubSleep(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryUntil(ctx, log, "dep", func(context.Context) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("down")
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, calls)
}

// A retryStop must end the loop on the first attempt. Without this, the
// wrapper is decoration: retryUntil never returns fn's error, so the
// errors.As unwrap at every call site is unreachable and an error the caller
// declared fatal is retried forever instead.
func TestRetryUntilStopsOnRetryStop(t *testing.T) {
	waits := stubSleep(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fatal := errors.New("the node is on a different chain")
	calls := 0

	err := retryUntil(t.Context(), log, "chain rpc", func(context.Context) error {
		calls++
		return retryStop{fatal}
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, fatal, "the caller must be able to unwrap what it wrapped")
	assert.Equal(t, 1, calls, "a fatal error must not be retried")
	assert.Empty(t, *waits, "and must not sleep before giving up")
}
