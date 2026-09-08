package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Handler processes one delivered event. Returning nil acks the message;
// returning an error naks it for redelivery after the consumer's backoff.
type Handler func(ctx context.Context, e Envelope) error

// ConsumerConfig declares a durable consumer (docs/plan-v1.0.md §7.3
// "processing" consumers: durable, explicit ack).
//
// Fan-out consumers — the WS servers of the stream role — must NOT use this:
// a shared durable consumer is a work queue, so each replica would receive
// only a slice of the events. They take an ordered ephemeral consumer and
// deduplicate on seq instead.
type ConsumerConfig struct {
	Durable        string        // consumer name; also the processed_events key
	Stream         string        // StreamTrading, StreamChain, StreamRegistry
	FilterSubjects []string      // at least one
	MaxDeliver     int           // default 10
	Backoff        time.Duration // nak delay, default 2s
	AckWait        time.Duration // default 30s
	Metrics        *Metrics      // optional: event_consumer_lag{consumer=Durable}
}

func (c ConsumerConfig) withDefaults() ConsumerConfig {
	if c.MaxDeliver <= 0 {
		c.MaxDeliver = 10
	}
	if c.Backoff <= 0 {
		c.Backoff = 2 * time.Second
	}
	if c.AckWait <= 0 {
		c.AckWait = 30 * time.Second
	}
	return c
}

func (c ConsumerConfig) validate() error {
	switch {
	case c.Durable == "":
		return errors.New("eventbus: consumer durable name required")
	case c.Stream == "":
		return errors.New("eventbus: consumer stream required")
	case len(c.FilterSubjects) == 0:
		return errors.New("eventbus: consumer needs at least one filter subject")
	}
	return nil
}

// Subscription is a running durable consumer.
type Subscription struct {
	cc   jetstream.ConsumeContext
	stop context.CancelFunc
	done sync.WaitGroup
}

// Stop ends the consumption loop; the durable consumer and its position
// survive on the server, so a restart resumes where this left off.
func (s *Subscription) Stop() {
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

// Subscribe creates (or reuses) the durable consumer and starts delivering
// envelopes to handler until Stop or ctx ends.
//
// The handler decides idempotency. A handler whose effect is idempotent by
// construction — the registry reload, which only re-reads the database —
// needs nothing more. One that writes must record MarkProcessed in the same
// transaction as its write and treat a duplicate as already done.
func Subscribe(ctx context.Context, js jetstream.JetStream, cfg ConsumerConfig, log *slog.Logger, h Handler) (*Subscription, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:        cfg.Durable,
		Name:           cfg.Durable,
		FilterSubjects: cfg.FilterSubjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverNewPolicy,
		MaxDeliver:     cfg.MaxDeliver,
		AckWait:        cfg.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: consumer %s on %s: %w", cfg.Durable, cfg.Stream, err)
	}
	log = log.With(slog.String("consumer", cfg.Durable), slog.String("stream", cfg.Stream))
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		env, err := Unmarshal(msg.Data())
		if err != nil {
			// A message that cannot be decoded will never decode: acking it
			// keeps one poison message from blocking the consumer forever.
			log.Error("undecodable event acked", slog.String("subject", msg.Subject()), slog.String("err", err.Error()))
			ack(log, msg)
			return
		}
		hctx, end := consumerSpan(ctx, msg, cfg.Durable, env)
		err = h(hctx, env)
		end(err)
		if err != nil {
			log.Error("event handler failed, redelivering",
				slog.String("event_id", env.EventID), slog.String("event_type", env.EventType), slog.String("err", err.Error()))
			if nerr := msg.NakWithDelay(cfg.Backoff); nerr != nil {
				log.Error("nak failed", slog.String("err", nerr.Error()))
			}
			return
		}
		ack(log, msg)
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: consume %s: %w", cfg.Durable, err)
	}
	sub := &Subscription{cc: cc}
	if cfg.Metrics != nil {
		sctx, cancel := context.WithCancel(ctx)
		sub.stop = cancel
		sub.done.Add(1)
		go func() {
			defer sub.done.Done()
			sampleLag(sctx, cons, cfg.Durable, cfg.Metrics)
		}()
	}
	log.Info("event consumer started", slog.Any("subjects", cfg.FilterSubjects))
	return sub, nil
}

func ack(log *slog.Logger, msg jetstream.Msg) {
	if err := msg.Ack(); err != nil {
		log.Error("ack failed", slog.String("err", err.Error()))
	}
}
