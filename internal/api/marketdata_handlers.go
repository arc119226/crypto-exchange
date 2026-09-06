package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// defaultDepth and maxDepth bound GET /depth.
const (
	defaultDepth = 20
	maxDepth     = 200
)

func toLevels(levels []matching.Level) []gen.Level {
	out := make([]gen.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, gen.Level{Price: l.Price, Qty: l.Qty, Orders: int32(l.Orders)}) //nolint:gosec // orders per level
	}
	return out
}

// GetDepth implements GET /v1/markets/{symbol}/depth.
func (h *Handler) GetDepth(ctx context.Context, req gen.GetDepthRequestObject) (gen.GetDepthResponseObject, error) {
	if h.d.Trading == nil {
		return gen.GetDepth503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "order book is not available in this role")}, nil
	}
	n := defaultDepth
	if req.Params.Limit != nil {
		n = int(*req.Params.Limit)
	}
	if n < 1 || n > maxDepth {
		n = defaultDepth
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

// correlationOf returns the request's correlation id for ledger / outbox rows.
func correlationOf(ctx context.Context) string { return telemetry.CorrelationID(ctx) }
