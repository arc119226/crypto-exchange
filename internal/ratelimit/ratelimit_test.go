package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLimit(t *testing.T) {
	l, err := ParseLimit("5/1m")
	require.NoError(t, err)
	assert.Equal(t, Limit{N: 5, Window: time.Minute}, l)
	assert.Equal(t, 12*time.Second, l.interval())
	for _, bad := range []string{"", "5", "0/1m", "-1/1m", "x/1m", "5/0s", "5/abc"} {
		_, err := ParseLimit(bad)
		assert.Error(t, err, bad)
	}
}

func TestMemoryTokenBucket(t *testing.T) {
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	m := NewMemory().WithClock(func() time.Time { return now })
	l := Limit{N: 5, Window: time.Minute} // one token every 12 s
	ctx := context.Background()

	// burst of 5, the 6th is denied (plan DoD: 6th failed login → 429)
	for i := 0; i < 5; i++ {
		d, err := m.Allow(ctx, "login:acct:alice", l)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "attempt %d", i+1)
		assert.Equal(t, int64(4-i), d.Remaining)
	}
	d, err := m.Allow(ctx, "login:acct:alice", l)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, 12*time.Second, d.RetryAfter)

	// other keys are independent
	d, err = m.Allow(ctx, "login:acct:bob", l)
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	// after one interval exactly one token is back
	now = now.Add(12 * time.Second)
	d, err = m.Allow(ctx, "login:acct:alice", l)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	d, err = m.Allow(ctx, "login:acct:alice", l)
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	// a long idle period refills to the cap, never beyond
	now = now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		d, err = m.Allow(ctx, "login:acct:alice", l)
		require.NoError(t, err)
		assert.True(t, d.Allowed)
	}
	d, err = m.Allow(ctx, "login:acct:alice", l)
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	_, err = m.Allow(ctx, "k", Limit{})
	assert.Error(t, err)
}

type failing struct{}

func (failing) Allow(context.Context, string, Limit) (Decision, error) {
	return Decision{}, errors.New("redis down")
}

func TestFallback(t *testing.T) {
	var seen error
	f := Fallback{Primary: failing{}, Secondary: NewMemory(), OnError: func(err error) { seen = err }}
	d, err := f.Allow(context.Background(), "k", Limit{N: 1, Window: time.Second})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.EqualError(t, seen, "redis down")
	d, err = f.Allow(context.Background(), "k", Limit{N: 1, Window: time.Second})
	require.NoError(t, err)
	assert.False(t, d.Allowed, "the fallback keeps counting")
}
