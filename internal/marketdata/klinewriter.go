package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arc119226/crypto-exchange/internal/registry"
)

// DefaultKlineBatchSeqs bounds how many engine commands one writer pass
// folds per market.
const DefaultKlineBatchSeqs = 500

// KlineWriter persists candles (docs/plan-v1.0.md §12 Phase 6: "worker
// 持久化 marketdata.klines;重啟從最後一根 K 線的 seq 重放").
//
// It polls the trade table rather than consuming trade.executed events: the
// table is the truth, is indexed by (market, seq, idx), and lets one
// transaction fold a whole batch and move the cursor together. A durable
// consumer would need one acknowledged round trip per trade to be as safe,
// because a redelivered trade after a later one was folded would have to be
// told apart from a duplicate.
type KlineWriter struct {
	store   *Store
	markets registry.Reader
	tenant  string
	batch   int64
	metrics *Metrics
	log     *slog.Logger
}

// NewKlineWriter builds a writer over store, folding at most batch commands
// per market per pass.
func NewKlineWriter(store *Store, markets registry.Reader, tenant string, batch int64, log *slog.Logger) *KlineWriter {
	if batch <= 0 {
		batch = DefaultKlineBatchSeqs
	}
	if log == nil {
		log = slog.Default()
	}
	return &KlineWriter{store: store, markets: markets, tenant: tenant, batch: batch, log: log}
}

// WithMetrics attaches instruments.
func (w *KlineWriter) WithMetrics(m *Metrics) *KlineWriter {
	w.metrics = m
	return w
}

// Tick folds one batch per market. caughtUp is false when some market still
// has commands past this batch; the caller then ticks again without waiting.
func (w *KlineWriter) Tick(ctx context.Context) (applied int, caughtUp bool, err error) {
	markets, err := w.markets.ListMarkets(ctx, w.tenant)
	if err != nil {
		return 0, true, fmt.Errorf("marketdata: markets: %w", err)
	}
	caughtUp = true
	var firstErr error
	for _, m := range markets {
		n, more, err := w.tickMarket(ctx, m)
		if err != nil {
			if ctx.Err() != nil {
				return applied, false, err
			}
			w.log.Error("kline writer: market failed", slog.String("market", m.Symbol), slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		applied += n
		if more {
			caughtUp = false
		}
	}
	return applied, caughtUp, firstErr
}

func (w *KlineWriter) tickMarket(ctx context.Context, m registry.Market) (int, bool, error) {
	cursor, err := w.store.KlineCursor(ctx, m.Symbol)
	if err != nil {
		return 0, false, err
	}
	last, err := w.store.MarketSeq(ctx, m.ID)
	if err != nil {
		return 0, false, err
	}
	w.metrics.observeKlineLag(m.Symbol, max(last-cursor, 0))
	if last <= cursor {
		return 0, false, nil
	}
	upto := min(last, cursor+w.batch)
	trades, err := w.store.TradesBetween(ctx, m.ID, cursor, upto)
	if err != nil {
		return 0, false, err
	}
	agg := NewAggregator(m.Symbol)
	buckets := map[Interval]map[time.Time]Candle{}
	for _, t := range trades {
		for _, c := range agg.Apply(t) {
			if buckets[c.Interval] == nil {
				buckets[c.Interval] = map[time.Time]Candle{}
			}
			buckets[c.Interval][c.Start] = c
		}
	}
	var candles []Candle
	for _, byStart := range buckets {
		for _, c := range byStart {
			candles = append(candles, c)
		}
	}
	if err := w.store.ApplyCandles(ctx, m.Symbol, candles, upto); err != nil {
		return 0, false, err
	}
	w.metrics.observeKlineLag(m.Symbol, last-upto)
	return len(trades), upto < last, nil
}

// Run ticks until ctx ends: back to back while behind, every interval once
// caught up.
func (w *KlineWriter) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		_, caughtUp, err := w.Tick(ctx)
		if err != nil && errors.Is(err, context.Canceled) {
			return nil
		}
		if !caughtUp && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
