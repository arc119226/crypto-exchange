//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// TestOutboxRelayPublishesToJetStream: every committed outbox row reaches
// the EX_TRADING stream exactly once, in id order, and a republish inside the
// duplicate window is dropped by the server (docs/plan-v1.0.md §7.3).
func TestOutboxRelayPublishesToJetStream(t *testing.T) {
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
	require.NoError(t, eventbus.EnsureStreams(ctx, js), "idempotent")
	stream, err := js.Stream(ctx, eventbus.StreamTrading)
	require.NoError(t, err)
	require.NoError(t, stream.Purge(ctx), "a shared local server may hold events of earlier runs")

	// produce: a maker, a taker that trades, a cancel
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "1"})
	h.place(t, ctx, limit(seller, "s1", matching.Sell, "1990", "0.4"))
	b1 := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2000", "1"))
	_, err = h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: buyer, OrderID: b1.Order.ID})
	require.NoError(t, err)
	rows := h.outbox(t, ctx)
	require.NotEmpty(t, rows)

	relay := eventbus.NewRelay(h.all, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{BatchSize: 3}, h.log)
	published := 0
	for {
		n, err := relay.Drain(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
		published += n
	}
	assert.Equal(t, len(rows), published)
	var unpublished int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM eventbus.outbox WHERE published_at IS NULL`).Scan(&unpublished))
	assert.Zero(t, unpublished)

	// consume in order and compare with the outbox
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{})
	require.NoError(t, err)
	batch, err := cons.Fetch(len(rows), jetstream.FetchMaxWait(10*time.Second))
	require.NoError(t, err)
	var got []eventbus.Envelope
	for msg := range batch.Messages() {
		env, err := eventbus.Unmarshal(msg.Data())
		require.NoError(t, err)
		assert.Equal(t, env.Subject(), msg.Subject())
		assert.Equal(t, env.EventID, msg.Headers().Get(jetstream.MsgIDHeader))
		got = append(got, env)
	}
	require.NoError(t, batch.Error())
	require.Len(t, got, len(rows))
	var lastSeq uint64
	for i, env := range got {
		assert.Equal(t, rows[i].EventType, env.EventType, "event %d", i)
		assert.Equal(t, rows[i].Subject, env.Subject(), "event %d", i)
		if env.Seq != nil {
			assert.GreaterOrEqual(t, *env.Seq, lastSeq, "market seq never goes backwards in publish order")
			lastSeq = *env.Seq
		}
		assert.Equal(t, 1, env.SchemaVersion)
		assert.Equal(t, "default", env.TenantID)
	}

	// republishing the same rows (a relay restart mid-batch) adds nothing
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	before := info.State.Msgs
	_, err = h.all.Exec(ctx, `UPDATE eventbus.outbox SET published_at = NULL`)
	require.NoError(t, err)
	for {
		n, err := relay.Drain(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	info, err = stream.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, info.State.Msgs, "Nats-Msg-Id deduplication")

	// the run loop wakes up on NOTIFY (or the poll) and publishes new rows
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()
	h.place(t, ctx, limit(seller, "s2", matching.Sell, "2100", "0.1"))
	require.Eventually(t, func() bool {
		var n int
		if err := h.all.QueryRow(ctx, `SELECT count(*) FROM eventbus.outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
			return false
		}
		return n == 0
	}, 5*time.Second, 50*time.Millisecond)
	stop()
	require.NoError(t, <-done)
	info, err = stream.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+2, info.State.Msgs, "order.accepted + balance.updated of s2")

	// MarkProcessed is the consumer-side idempotency primitive
	tx, err := h.all.Begin(ctx)
	require.NoError(t, err)
	first, err := eventbus.MarkProcessed(ctx, tx, "test-consumer", got[0].EventID)
	require.NoError(t, err)
	second, err := eventbus.MarkProcessed(ctx, tx, "test-consumer", got[0].EventID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	assert.True(t, first)
	assert.False(t, second)
}
