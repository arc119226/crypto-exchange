package app

import (
	"context"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// webhookSubs are the durable consumers feeding the delivery queue, one per
// stream.
//
// This lives in app rather than in internal/webhook for the reason signerbus
// lives outside internal/chain/signer: a domain package does not depend on
// NATS. webhook signs and speaks HTTP; which transport the events arrived on
// is this layer's problem, and depguard enforces it.
type webhookSubs []*eventbus.Subscription

// Stop ends every consume loop. Durable position lives on the server, so a
// restart resumes where this left off.
func (s webhookSubs) Stop() {
	for _, sub := range s {
		sub.Stop()
	}
}

// streamDomains is which event domains live in which stream. It mirrors
// eventbus.StreamConfigs, and has to: ConsumerConfig.Stream is a single
// string, so an endpoint subscribing to both trade.executed and
// deposit.credited needs a consumer on each of two streams.
var webhookStreamDomains = map[string][]string{
	eventbus.StreamTrading:  {"order", "trade", "ledger", "balance"},
	eventbus.StreamChain:    {"deposit", "withdrawal", "sweep", "alert"},
	eventbus.StreamRegistry: {"market", "asset", "fee_schedule", "user", "reconciliation"},
}

// consumeForWebhooks subscribes the dispatcher to every stream.
//
// The handler is Enqueue and nothing else, so a redelivery can only re-run an
// idempotent insert. That is what makes it safe to ack immediately, which in
// turn is what lets the retry schedule be ours rather than JetStream's --
// §7.6 wants waits of up to 24 hours, and a nak delay is one flat value
// inside a 30-second ack window.
func consumeForWebhooks(ctx context.Context, js jetstream.JetStream, d *webhook.Dispatcher, tenant, durablePrefix string, log *slog.Logger) (webhookSubs, error) {
	var subs webhookSubs
	for _, stream := range []string{eventbus.StreamTrading, eventbus.StreamChain, eventbus.StreamRegistry} {
		filters := make([]string, 0, len(webhookStreamDomains[stream]))
		for _, domain := range webhookStreamDomains[stream] {
			filters = append(filters, eventbus.SubjectPrefix+"."+domain+".*."+tenant+".*")
		}
		sub, err := eventbus.Subscribe(ctx, js, eventbus.ConsumerConfig{
			Durable: durablePrefix + "-" + webhookStreamSuffix(stream), Stream: stream, FilterSubjects: filters,
		}, log, d.Enqueue)
		if err != nil {
			subs.Stop()
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// webhookStreamSuffix turns EX_TRADING into trading, so a durable reads
// worker-webhook-trading.
func webhookStreamSuffix(stream string) string {
	switch stream {
	case eventbus.StreamTrading:
		return "trading"
	case eventbus.StreamChain:
		return "chain"
	default:
		return "registry"
	}
}
