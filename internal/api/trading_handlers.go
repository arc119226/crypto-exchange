package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

func toOrder(o trading.Order) gen.Order {
	out := gen.Order{
		ID: o.ID, ClientOrderID: o.ClientOrderID, Market: o.MarketSymbol,
		Side: gen.Side(o.Side.String()), Type: gen.OrderType(o.Type.String()), TimeInForce: gen.TimeInForce(o.TimeInForce.String()),
		Price: o.Price, Qty: o.Qty, QuoteQty: o.QuoteQty,
		FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, RemainingQty: o.RemainingQty,
		HoldAsset: o.HoldAsset, HoldAmount: o.HoldAmount, HoldRemaining: o.HoldRemaining,
		Status: gen.OrderStatus(o.Status), RejectReason: string(o.RejectReason), CancelReason: string(o.CancelReason),
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
	if o.Seq != nil {
		seq := int64(*o.Seq) //nolint:gosec // engine seq
		out.Seq = &seq
	}
	return out
}

func toTrade(t trading.Trade) gen.Trade {
	return gen.Trade{
		TradeID: t.ID, Market: t.MarketSymbol, Price: t.Price, Qty: t.Qty, QuoteQty: t.QuoteQty,
		TakerSide: gen.Side(t.TakerSide.String()), Seq: int64(t.Seq), ExecutedAt: t.CreatedAt, //nolint:gosec // engine seq
	}
}

func toFill(f trading.Fill) gen.Fill {
	t := toTrade(f.Trade)
	return gen.Fill{
		TradeID: t.TradeID, Market: t.Market, Price: t.Price, Qty: t.Qty, QuoteQty: t.QuoteQty, TakerSide: t.TakerSide, Seq: t.Seq, ExecutedAt: t.ExecutedAt,
		OrderID: f.OrderID, Side: gen.Side(f.Side.String()), IsMaker: f.IsMaker, Fee: f.Fee, FeeAsset: f.FeeAsset,
	}
}

// fillsOf projects trades onto the account's side.
func fillsOf(trades []trading.Trade, accountID string) []gen.Fill {
	out := make([]gen.Fill, 0, len(trades))
	for _, t := range trades {
		if f, ok := trading.FillOf(t, accountID); ok {
			out = append(out, toFill(f))
		}
	}
	return out
}

func amountOrZero(a *money.Amount) money.Amount {
	if a == nil {
		return money.Zero
	}
	return *a
}

// PlaceOrder implements POST /v1/orders.
func (h *Handler) PlaceOrder(ctx context.Context, req gen.PlaceOrderRequestObject) (gen.PlaceOrderResponseObject, error) {
	if h.d.Trading == nil {
		return gen.PlaceOrder503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "trading is not available in this role")}, nil
	}
	p, err := requireScope(ctx, auth.ScopeTrade)
	if errors.Is(err, errUnauthenticated) {
		return gen.PlaceOrder401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	if err != nil {
		return gen.PlaceOrder403ApplicationProblemPlusJSONResponse{ForbiddenApplicationProblemPlusJSONResponse: h.forbidden(ctx, "the trade scope is required")}, nil
	}
	if req.Body == nil {
		return gen.PlaceOrder400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "missing body")}, nil
	}
	if d, err := h.allow(ctx, "orders:acct:"+p.AccountID, h.d.Limits.OrdersPerAccount); err != nil {
		return nil, err
	} else if !d.Allowed {
		return gen.PlaceOrder429ApplicationProblemPlusJSONResponse{TooManyRequestsApplicationProblemPlusJSONResponse: h.tooMany(ctx, d)}, nil
	}
	var side matching.Side
	if err := side.UnmarshalText([]byte(req.Body.Side)); err != nil {
		return gen.PlaceOrder400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "side must be buy or sell")}, nil
	}
	var typ matching.OrderType
	if err := typ.UnmarshalText([]byte(req.Body.Type)); err != nil {
		return gen.PlaceOrder400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "type must be limit or market")}, nil
	}
	var tif matching.TimeInForce
	if req.Body.TimeInForce != nil {
		if err := tif.UnmarshalText([]byte(*req.Body.TimeInForce)); err != nil {
			return gen.PlaceOrder400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "time_in_force must be gtc or ioc")}, nil
		}
	}
	res, err := h.d.Trading.PlaceOrder(ctx, trading.PlaceOrderRequest{
		AccountID: p.AccountID, MarketSymbol: req.Body.Market, ClientOrderID: req.Body.ClientOrderID,
		Side: side, Type: typ, TimeInForce: tif,
		Price: amountOrZero(req.Body.Price), Qty: amountOrZero(req.Body.Qty), QuoteQty: amountOrZero(req.Body.QuoteQty),
		CorrelationID: correlationOf(ctx),
	})
	switch {
	case errors.Is(err, trading.ErrInvalidRequest):
		return gen.PlaceOrder400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, strings.TrimPrefix(err.Error(), "trading: invalid request: "))}, nil
	case errors.Is(err, trading.ErrMarketNotFound):
		return gen.PlaceOrder404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Body.Market+" does not exist")}, nil
	case errors.Is(err, trading.ErrClientOrderIDMismatch):
		return gen.PlaceOrder422ApplicationProblemPlusJSONResponse{UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx, "client_order_id "+req.Body.ClientOrderID+" was already used with a different order")}, nil
	case errors.Is(err, trading.ErrEngineUnavailable):
		return gen.PlaceOrder503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "trading engine unavailable; retry with the same client_order_id")}, nil
	case err != nil:
		return nil, fmt.Errorf("place order: %w", err)
	}
	result := gen.OrderResult{Order: toOrder(res.Order), Trades: fillsOf(res.Trades, p.AccountID)}
	if res.Replayed {
		return gen.PlaceOrder200JSONResponse(result), nil
	}
	return gen.PlaceOrder201JSONResponse(result), nil
}

// ListOrders implements GET /v1/orders.
func (h *Handler) ListOrders(ctx context.Context, req gen.ListOrdersRequestObject) (gen.ListOrdersResponseObject, error) {
	if h.d.Trading == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListOrders401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	f := trading.OrdersFilter{AccountID: p.AccountID, Limit: limit, Offset: offset}
	if req.Params.Market != nil {
		f.Market = *req.Params.Market
	}
	if req.Params.Status != nil {
		f.Status = trading.Status(*req.Params.Status)
	}
	if req.Params.OpenOnly != nil {
		f.OpenOnly = *req.Params.OpenOnly
	}
	orders, err := h.d.Trading.ListOrders(ctx, f)
	if errors.Is(err, trading.ErrInvalidRequest) {
		return gen.ListOrders400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, strings.TrimPrefix(err.Error(), "trading: invalid request: "))}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	out := make([]gen.Order, 0, len(orders))
	for _, o := range orders {
		out = append(out, toOrder(o))
	}
	return gen.ListOrders200JSONResponse(gen.OrderList{Orders: out}), nil
}

// GetOrder implements GET /v1/orders/{id}.
func (h *Handler) GetOrder(ctx context.Context, req gen.GetOrderRequestObject) (gen.GetOrderResponseObject, error) {
	if h.d.Trading == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.GetOrder401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	o, err := h.d.Trading.GetOrder(ctx, p.AccountID, req.ID)
	if errors.Is(err, trading.ErrOrderNotFound) {
		return gen.GetOrder404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "order "+req.ID+" does not exist")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	trades, err := h.d.Trading.OrderTrades(ctx, p.AccountID, req.ID)
	if err != nil {
		return nil, fmt.Errorf("order trades: %w", err)
	}
	return gen.GetOrder200JSONResponse(gen.OrderResult{Order: toOrder(o), Trades: fillsOf(trades, p.AccountID)}), nil
}

// CancelOrder implements DELETE /v1/orders/{id}.
func (h *Handler) CancelOrder(ctx context.Context, req gen.CancelOrderRequestObject) (gen.CancelOrderResponseObject, error) {
	if h.d.Trading == nil {
		return gen.CancelOrder503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "trading is not available in this role")}, nil
	}
	p, err := requireScope(ctx, auth.ScopeTrade)
	if errors.Is(err, errUnauthenticated) {
		return gen.CancelOrder401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	if err != nil {
		return gen.CancelOrder403ApplicationProblemPlusJSONResponse{ForbiddenApplicationProblemPlusJSONResponse: h.forbidden(ctx, "the trade scope is required")}, nil
	}
	if d, err := h.allow(ctx, "orders:acct:"+p.AccountID, h.d.Limits.OrdersPerAccount); err != nil {
		return nil, err
	} else if !d.Allowed {
		return gen.CancelOrder429ApplicationProblemPlusJSONResponse{TooManyRequestsApplicationProblemPlusJSONResponse: h.tooMany(ctx, d)}, nil
	}
	o, err := h.d.Trading.CancelOrder(ctx, trading.CancelRequest{AccountID: p.AccountID, OrderID: req.ID, CorrelationID: correlationOf(ctx)})
	switch {
	case errors.Is(err, trading.ErrOrderNotFound):
		return gen.CancelOrder404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "order "+req.ID+" does not exist")}, nil
	case errors.Is(err, trading.ErrMarketNotFound):
		return gen.CancelOrder404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "the order's market no longer accepts cancels")}, nil
	case errors.Is(err, trading.ErrEngineUnavailable):
		return gen.CancelOrder503ApplicationProblemPlusJSONResponse{ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "trading engine unavailable; retry")}, nil
	case err != nil:
		return nil, fmt.Errorf("cancel order: %w", err)
	}
	return gen.CancelOrder200JSONResponse(toOrder(o)), nil
}

// ListFills implements GET /v1/fills.
func (h *Handler) ListFills(ctx context.Context, req gen.ListFillsRequestObject) (gen.ListFillsResponseObject, error) {
	if h.d.Trading == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListFills401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	f := trading.FillsFilter{AccountID: p.AccountID, Limit: limit, Offset: offset}
	if req.Params.Market != nil {
		f.Market = *req.Params.Market
	}
	if req.Params.OrderID != nil {
		f.OrderID = *req.Params.OrderID
	}
	fills, err := h.d.Trading.ListFills(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list fills: %w", err)
	}
	out := make([]gen.Fill, 0, len(fills))
	for _, fl := range fills {
		out = append(out, toFill(fl))
	}
	return gen.ListFills200JSONResponse(gen.FillList{Fills: out}), nil
}
