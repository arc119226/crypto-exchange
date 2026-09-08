package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// defaultDepth and maxDepth bound GET /depth.
const (
	defaultDepth = 20
	maxDepth     = 200
)

// Kline paging.
const (
	defaultKlines = 500
	maxKlines     = 1000
)

func toLevels(levels []matching.Level) []gen.Level {
	out := make([]gen.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, gen.Level{Price: l.Price, Qty: l.Qty, Orders: int32(l.Orders)}) //nolint:gosec // orders per level
	}
	return out
}

func toCachedLevels(levels []marketdata.Level, n int) []gen.Level {
	if n > 0 && n < len(levels) {
		levels = levels[:n]
	}
	out := make([]gen.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, gen.Level{Price: l.Price, Qty: l.Qty, Orders: int32(l.Orders)}) //nolint:gosec // orders per level
	}
	return out
}

// GetDepth implements GET /v1/markets/{symbol}/depth.
//
// The stream role keeps a snapshot of every book in the cache; when that is
// fresh it is served as is, so a thousand clients reconnecting after a blip
// do not turn into a thousand request-replies at the market's runner. The
// engine answers when there is no cache, no snapshot, or a stale one.
func (h *Handler) GetDepth(ctx context.Context, req gen.GetDepthRequestObject) (gen.GetDepthResponseObject, error) {
	n := defaultDepth
	if req.Params.Limit != nil {
		n = int(*req.Params.Limit)
	}
	if n < 1 || n > maxDepth {
		n = defaultDepth
	}
	if _, err := h.d.Registry.GetMarket(ctx, h.d.Tenant, req.Symbol); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return gen.GetDepth404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
		}
		return nil, fmt.Errorf("depth: market: %w", err)
	}
	if d, ok := h.cachedDepth(ctx, req.Symbol); ok {
		return gen.GetDepth200JSONResponse(gen.Depth{
			Market: d.Market, LastSeq: int64(d.Seq), Bids: toCachedLevels(d.Bids, n), Asks: toCachedLevels(d.Asks, n), //nolint:gosec // engine seq
		}), nil
	}
	if h.d.Trading == nil {
		return gen.GetDepth503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "order book is not available in this role")}, nil
	}
	depth, err := h.d.Trading.Depth(ctx, req.Symbol, n)
	switch {
	case errors.Is(err, trading.ErrMarketNotFound):
		return gen.GetDepth404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
	case errors.Is(err, trading.ErrEngineUnavailable):
		return gen.GetDepth503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "trading engine unavailable")}, nil
	case err != nil:
		return nil, fmt.Errorf("depth: %w", err)
	}
	return gen.GetDepth200JSONResponse(gen.Depth{
		Market: depth.Symbol, LastSeq: int64(depth.LastSeq), Bids: toLevels(depth.Bids), Asks: toLevels(depth.Asks), //nolint:gosec // engine seq
	}), nil
}

// cachedDepth returns the cached snapshot of a market when the cache is
// configured, holds one, and it is younger than the freshness bound. A
// cache failure is logged and treated as a miss: the engine still answers.
func (h *Handler) cachedDepth(ctx context.Context, market string) (marketdata.Depth, bool) {
	if h.d.DepthCache == nil {
		return marketdata.Depth{}, false
	}
	d, at, ok, err := h.d.DepthCache.GetDepth(ctx, market)
	if err != nil {
		telemetry.Logger(ctx).Warn("depth cache unavailable, asking the engine", "err", err.Error())
		return marketdata.Depth{}, false
	}
	if !ok || h.now().Sub(at) > h.depthFreshness() {
		return marketdata.Depth{}, false
	}
	return d, true
}

func (h *Handler) depthFreshness() time.Duration {
	if h.d.DepthFreshness > 0 {
		return h.d.DepthFreshness
	}
	return 10 * time.Second
}

func (h *Handler) now() time.Time {
	if h.d.Now != nil {
		return h.d.Now()
	}
	return time.Now()
}

// ListTrades implements GET /v1/markets/{symbol}/trades.
func (h *Handler) ListTrades(ctx context.Context, req gen.ListTradesRequestObject) (gen.ListTradesResponseObject, error) {
	if h.d.Trading == nil {
		return nil, errUnavailable
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	trades, err := h.d.Trading.ListTrades(ctx, req.Symbol, limit, offset)
	if errors.Is(err, trading.ErrMarketNotFound) {
		return gen.ListTrades404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list trades: %w", err)
	}
	out := make([]gen.Trade, 0, len(trades))
	for _, t := range trades {
		out = append(out, toTrade(t))
	}
	return gen.ListTrades200JSONResponse(gen.TradeList{Trades: out}), nil
}

// GetTicker implements GET /v1/markets/{symbol}/ticker.
func (h *Handler) GetTicker(ctx context.Context, req gen.GetTickerRequestObject) (gen.GetTickerResponseObject, error) {
	if h.d.MarketData == nil {
		return gen.GetTicker503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "market data is not available in this role")}, nil
	}
	if _, err := h.d.Registry.GetMarket(ctx, h.d.Tenant, req.Symbol); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return gen.GetTicker404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
		}
		return nil, fmt.Errorf("ticker: market: %w", err)
	}
	tk, err := h.d.MarketData.Ticker(ctx, req.Symbol, h.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("ticker: %w", err)
	}
	return gen.GetTicker200JSONResponse(gen.Ticker{
		Market: tk.Market, LastPrice: tk.Last, Open: tk.Open, High: tk.High, Low: tk.Low, Change: tk.Change, ChangePct: tk.ChangePct,
		Volume: tk.Volume, QuoteVolume: tk.QuoteVolume, Trades: tk.Trades, At: tk.At,
	}), nil
}

// ListKlines implements GET /v1/markets/{symbol}/klines.
func (h *Handler) ListKlines(ctx context.Context, req gen.ListKlinesRequestObject) (gen.ListKlinesResponseObject, error) {
	if h.d.MarketData == nil {
		return gen.ListKlines503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "market data is not available in this role")}, nil
	}
	iv, err := marketdata.ParseInterval(string(req.Params.Interval))
	if err != nil {
		return gen.ListKlines400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "interval must be one of 1m, 5m, 15m, 1h, 1d")}, nil
	}
	limit := int32(defaultKlines)
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 || limit > maxKlines {
		return gen.ListKlines400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, fmt.Sprintf("limit must be between 1 and %d", maxKlines))}, nil
	}
	to := h.now().UTC()
	if req.Params.To != nil {
		to = req.Params.To.UTC()
	}
	// the default window is the last `limit` buckets including the one `to`
	// falls in, so a chart asking for 500 candles gets the live one too
	from := iv.BucketStart(to).Add(-time.Duration(limit-1) * iv.Duration())
	if req.Params.From != nil {
		from = iv.BucketStart(req.Params.From.UTC())
	}
	if !to.After(from) {
		return gen.ListKlines400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "from must be before to")}, nil
	}
	if cut := from.Add(time.Duration(limit) * iv.Duration()); cut.Before(to) {
		to = cut // limit buckets from `from`
	}
	if _, err := h.d.Registry.GetMarket(ctx, h.d.Tenant, req.Symbol); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return gen.ListKlines404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
		}
		return nil, fmt.Errorf("klines: market: %w", err)
	}
	stored, err := h.d.MarketData.Klines(ctx, req.Symbol, iv, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("klines: %w", err)
	}
	prevClose, err := h.d.MarketData.LastCloseBefore(ctx, req.Symbol, iv, from)
	if err != nil {
		return nil, fmt.Errorf("klines: previous close: %w", err)
	}
	candles := marketdata.FillGaps(stored, req.Symbol, iv, from, to, prevClose)
	out := make([]gen.Kline, 0, len(candles))
	for _, c := range candles {
		out = append(out, gen.Kline{
			Market: c.Market, Interval: gen.KlineInterval(c.Interval), Start: c.Start,
			Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume, QuoteVolume: c.QuoteVolume, Trades: c.Trades,
		})
	}
	return gen.ListKlines200JSONResponse(gen.KlineList{Market: req.Symbol, Interval: gen.KlineInterval(iv), Klines: out}), nil
}

// correlationOf returns the request's correlation id for ledger / outbox rows.
func correlationOf(ctx context.Context) string { return telemetry.CorrelationID(ctx) }
