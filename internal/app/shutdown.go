package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// shutdownStep is one thing to stop, in the order it appears in the plan.
type shutdownStep struct {
	name string
	stop func(ctx context.Context) error
}

// shutdownPlan is how a process comes down (docs/plan-v1.0.md §11):
//
//  1. readiness turns red (drain) and the process waits DrainDelay so a load
//     balancer stops sending it requests;
//  2. the HTTP servers finish their in-flight requests;
//  3. the command bus stops taking commands off NATS, then the engine stops:
//     the group it is committing finishes, the queue is answered
//     "unavailable"; the relay stops after it so that group's outbox rows
//     leave the process;
//  4. the stream closes its connections (1001 going away);
//  5. the ops server goes last, so /readyz answers "draining" throughout.
//
// The steps share one Timeout. Before Phase 7 the engine stopped the moment
// the context was cancelled, in parallel with the drain: every command
// queued during the drain window was refused, and an api request that had
// already been accepted could lose its engine under it.
type shutdownPlan struct {
	drain   func()
	delay   time.Duration
	timeout time.Duration
	steps   []shutdownStep
}

// run executes the plan and returns the first step's error.
func (p shutdownPlan) run(log *slog.Logger) error {
	log.Info("shutting down", slog.Duration("drain_delay", p.delay), slog.Duration("timeout", p.timeout))
	if p.drain != nil {
		p.drain()
	}
	time.Sleep(p.delay)
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	var first error
	for _, s := range p.steps {
		start := time.Now()
		if err := s.stop(ctx); err != nil {
			log.Warn("shutdown step failed", slog.String("step", s.name), slog.String("err", err.Error()))
			if first == nil {
				first = fmt.Errorf("shutdown %s: %w", s.name, err)
			}
			continue
		}
		log.Debug("shutdown step done", slog.String("step", s.name), slog.Duration("took", time.Since(start)))
	}
	return first
}
