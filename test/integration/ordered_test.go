//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/matching"
)

// TestOrderedConsumerDeliversEverythingAfterCreation: a fan-out consumer
// sees every event published after it was created, in stream order, and
// none from before (docs/events.md "Consumers": the subscriber takes a
// database snapshot after subscribing and drops what the snapshot already
// holds). The outbox reader returns an account's events by account_seq for
// the private stream's resume.
func TestOrderedConsumerDeliversEverythingAfterCreation(t *testing.T) {
	h := setupTrading(t)
	url := startNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Drain() //nolint:errcheck
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, eventbus.EnsureStreams(ctx, js))
	stream, err := js.Stream(ctx, eventbus.StreamTrading)
	require.NoError(t, err)
	require.NoError(t, stream.Purge(ctx))

	reg := prometheus.NewRegistry()
	relay := eventbus.NewRelay(h.all, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{BatchSize: 10}, h.log).WithMetrics(eventbus.NewMetrics(reg))
	drain := func() {
		for {
			n, err := relay.Drain(ctx)
			require.NoError(t, err)
			if n == 0 {
				return
			}
		}
	}

	// before the consumer exists: a resting order, published
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "1"})
	h.place(t, ctx, limit(seller, "s1", matching.Sell, "1990", "0.4"))
	drain()

	var (
		mu  sync.Mutex
		got []eventbus.Envelope
	)
	sub, err := eventbus.SubscribeOrdered(ctx, js, eventbus.OrderedConfig{
		Name: "test-fanout", Stream: eventbus.StreamTrading,
		FilterSubjects: []string{eventbus.SubjectPrefix + ".order.*.default.*", eventbus.SubjectPrefix + ".trade.*.default.*"},
		Metrics:        eventbus.NewMetrics(prometheus.NewRegistry()),
	}, h.log, func(_ context.Context, e eventbus.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	require.NoError(t, err)
	defer sub.Stop()

	// after: a taker that trades, then a cancel
	b1 := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2000", "1"))
	require.Len(t, b1.Trades, 1)
	rows := h.outbox(t, ctx)
	drain()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 4 // accepted(b1), trade, filled(s1), updated(b1); balances are filtered out
	}, 10*time.Second, 50*time.Millisecond)

	mu.Lock()
	types := make([]string, 0, len(got))
	for _, e := range got {
		types = append(types, e.EventType)
	}
	mu.Unlock()
	assert.Equal(t, []string{"order.accepted", "trade.executed", "order.filled", "order.updated"}, types, "stream order = outbox order, nothing from before the subscription")
	for _, e := range got {
		require.NotNil(t, e.Seq)
		assert.Equal(t, uint64(2), *e.Seq, "all four belong to the second command")
	}
	var marketRows int
	for _, r := range rows {
		if r.Seq != nil {
			marketRows++
		}
	}
	assert.Equal(t, 5, marketRows, "s1: accepted; b1: accepted, trade, filled, updated -- balances carry no seq")

	// the private stream's resume reads the same rows back by account_seq
	reader := eventbus.NewOutboxReader(h.all)
	replay, err := reader.ByAccountSince(ctx, "default", buyer, 0, 500)
	require.NoError(t, err)
	require.NotEmpty(t, replay)
	var last int64
	for _, e := range replay {
		require.NotNil(t, e.AccountSeq)
		assert.Greater(t, *e.AccountSeq, last, "ascending, no duplicates")
		last = *e.AccountSeq
		assert.Equal(t, buyer, *e.AccountID)
	}
	page, err := reader.ByAccountSince(ctx, "default", buyer, *replay[0].AccountSeq, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, replay[1].EventID, page[0].EventID, "since is exclusive, pages continue in order")
	none, err := reader.ByAccountSince(ctx, "default", buyer, last, 500)
	require.NoError(t, err)
	assert.Empty(t, none)

	sub.Stop()
	assert.NotNil(t, reg)
}
