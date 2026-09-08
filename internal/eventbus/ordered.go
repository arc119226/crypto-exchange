package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// lagSampleInterval is how often a consumer's pending count is read into
// event_consumer_lag.
const lagSampleInterval = 5 * time.Second

// OrderedHandler processes one delivered event. There is nothing to ack:
// an ordered consumer is a private, in-order view of the stream.
type OrderedHandler func(ctx context.Context, e Envelope)

// OrderedConfig declares a fan-out consumer (docs/plan-v1.0.md §7.3,
// docs/events.md "Consumers"): every replica gets every event, from the
// moment it subscribes, in stream order. Nothing is durable -- a restart
// starts from "now" and the subscriber rebuilds its state from Postgres
// (the shadow book from trading.orders, candles from marketdata.klines).
type OrderedConfig struct {
	// Name labels the consumer in logs and in event_consumer_lag; it does
	// not name the server-side consumer, which is ephemeral.
	Name           string
	Stream         string   // StreamTrading, StreamChain, StreamRegistry
	FilterSubjects []string // at least one
	Metrics        *Metrics // optional
}

func (c OrderedConfig) validate() error {
	switch {
	case c.Name == "":
		return errors.New("eventbus: ordered consumer name required")
	case c.Stream == "":
		return errors.New("eventbus: ordered consumer stream required")
	case len(c.FilterSubjects) == 0:
		return errors.New("eventbus: ordered consumer needs at least one filter subject")
	}
	return nil
}

// OrderedSubscription is a running ordered consumer.
type OrderedSubscription struct {
	cc   jetstream.ConsumeContext
	stop context.CancelFunc
	done sync.WaitGroup
}

// Stop ends delivery. The server-side consumer is ephemeral and is garbage
// collected; nothing is resumed on a restart.
func (s *OrderedSubscription) Stop() {
	if s == nil {
		return
	}
	if s.stop != nil {
		s.stop()
	}
	if s.cc != nil {
		s.cc.Stop()
	}
	s.done.Wait()
}

// SubscribeOrdered creates an ordered consumer that delivers every event
// published after this call, in order, to h.
//
// The consumer exists on the server when this returns, so the caller can
// take its database snapshot afterwards knowing that nothing committed in
// between is lost: events land in the handler, and the subscriber drops
// the ones the snapshot already contains by seq. On a delivery gap the
// client library recreates the consumer from the last delivered message;
// only a message the stream itself no longer holds is missed, which the
// subscriber's own sequence check catches.
func SubscribeOrdered(ctx context.Context, js jetstream.JetStream, cfg OrderedConfig, log *slog.Logger, h OrderedHandler) (*OrderedSubscription, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("consumer", cfg.Name), slog.String("stream", cfg.Stream))
	cons, err := js.OrderedConsumer(ctx, cfg.Stream, jetstream.OrderedConsumerConfig{
		FilterSubjects: cfg.FilterSubjects,
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: ordered consumer %s on %s: %w", cfg.Name, cfg.Stream, err)
	}
	sctx, cancel := context.WithCancel(ctx)
	sub := &OrderedSubscription{stop: cancel}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		env, err := Unmarshal(msg.Data())
		if err != nil {
			log.Error("undecodable event skipped", slog.String("subject", msg.Subject()), slog.String("err", err.Error()))
			return
		}
		hctx, end := consumerSpan(sctx, msg, cfg.Name, env)
		h(hctx, env)
		end(nil)
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("eventbus: consume %s: %w", cfg.Name, err)
	}
	sub.cc = cc
	if cfg.Metrics != nil {
		sub.done.Add(1)
		go func() {
			defer sub.done.Done()
			sampleLag(sctx, cons, cfg.Name, cfg.Metrics)
		}()
	}
	log.Info("ordered event consumer started", slog.Any("subjects", cfg.FilterSubjects))
	return sub, nil
}

// consumerSpan opens the consumer span for one delivery, parented on the
// traceparent the relay copied from the outbox row, and puts the message's
// correlation id in the context for the handler's logs.
func consumerSpan(ctx context.Context, msg jetstream.Msg, consumer string, env Envelope) (context.Context, func(error)) {
	hdr := map[string]string{}
	for k, vs := range msg.Headers() {
		if len(vs) > 0 {
			hdr[k] = vs[0]
		}
	}
	if cid := hdr[HeaderCorrelationID]; cid != "" && telemetry.CorrelationID(ctx) == "" {
		ctx = telemetry.WithCorrelationID(ctx, cid)
	}
	ctx = telemetry.ExtractTrace(ctx, hdr)
	return telemetry.StartSpan(ctx, "consume "+env.EventType, telemetry.SpanConsumer, map[string]string{
		"messaging.system":           "nats",
		"messaging.destination.name": msg.Subject(),
		"messaging.consumer.name":    consumer,
		"exchange.event_id":          env.EventID,
	})
}

// sampleLag reads the consumer's pending count into event_consumer_lag
// until ctx ends. An ordered consumer has no server-side instance until its
// first message; that is reported as no lag, which is also true.
func sampleLag(ctx context.Context, cons jetstream.Consumer, name string, m *Metrics) {
	tick := time.NewTicker(lagSampleInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := cons.Info(ictx)
		cancel()
		switch {
		case errors.Is(err, jetstream.ErrOrderedConsumerNotCreated):
			m.observeLag(name, 0)
		case err != nil:
			continue
		default:
			m.observeLag(name, info.NumPending)
		}
	}
}
