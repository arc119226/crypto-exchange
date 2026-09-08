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

// TestShutdownPlanOrder: readiness goes red first, the drain delay passes,
// then the steps run in order and one failing step neither stops the rest
// nor hides its error.
func TestShutdownPlanOrder(t *testing.T) {
	var order []string
	var drained time.Time
	step := func(name string, err error) shutdownStep {
		return shutdownStep{name: name, stop: func(ctx context.Context) error {
			require.NoError(t, ctx.Err(), "the shared budget is not spent")
			order = append(order, name)
			return err
		}}
	}
	p := shutdownPlan{
		drain:   func() { drained = time.Now(); order = append(order, "drain") },
		delay:   30 * time.Millisecond,
		timeout: time.Second,
		steps:   []shutdownStep{step("servers", nil), step("engine", errors.New("boom")), step("stream", nil)},
	}
	start := time.Now()
	err := p.run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.EqualError(t, err, "shutdown engine: boom")
	assert.Equal(t, []string{"drain", "servers", "engine", "stream"}, order)
	assert.GreaterOrEqual(t, time.Since(drained), 30*time.Millisecond, "the steps waited for the drain delay")
	assert.Less(t, time.Since(start), time.Second)
}

// TestShutdownPlanBudgetIsShared: a step that hangs eats the budget, the
// later steps see an expired context rather than waiting forever.
func TestShutdownPlanBudgetIsShared(t *testing.T) {
	var saw []error
	p := shutdownPlan{
		timeout: 50 * time.Millisecond,
		steps: []shutdownStep{
			{name: "slow", stop: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
			{name: "next", stop: func(ctx context.Context) error { saw = append(saw, ctx.Err()); return nil }},
		},
	}
	err := p.run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, saw, 1)
	assert.ErrorIs(t, saw[0], context.DeadlineExceeded)
}
