package trading

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading/sqlcgen"
)

// Service is the API-facing side of trading: it validates requests, routes
// commands to the engine through the bus and serves reads from Postgres
// (which any role may do; only the engine writes).
type Service struct {
	pool   *pgxpool.Pool
	bus    CommandBus
	reg    *registry.Cache
	tenant string
}

// NewService builds the service over a read pool and a command bus.
func NewService(pool *pgxpool.Pool, bus CommandBus, reg *registry.Cache, tenant string) *Service {
	return &Service{pool: pool, bus: bus, reg: reg, tenant: tenant}
}

// PlaceOrder validates the request's shape and market, then submits it.
// Business rejections come back as an order in StatusRejected.
func (s *Service) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error) {
	if err := req.Validate(); err != nil {
		return PlaceOrderResult{}, err
	}
	m, ok := s.reg.Market(req.MarketSymbol)
	if !ok || m.Status == registry.MarketDelisted {
		return PlaceOrderResult{}, fmt.Errorf("%w: %s", ErrMarketNotFound, req.MarketSymbol)
	}
	return s.bus.PlaceOrder(ctx, req)
}

// CancelOrder cancels one of the account's orders. Terminal orders are
// returned unchanged; unknown or foreign orders are ErrOrderNotFound.
func (s *Service) CancelOrder(ctx context.Context, req CancelRequest) (Order, error) {
	if req.AccountID == "" || req.OrderID == "" {
		return Order{}, fmt.Errorf("%w: account_id and order_id required", ErrInvalidRequest)
	}
	o, err := s.GetOrder(ctx, req.AccountID, req.OrderID)
	if err != nil {
		return Order{}, err
	}
	if o.Status.Terminal() {
		return o, nil
	}
	return s.bus.CancelOrder(ctx, o.MarketSymbol, req)
}

// GetOrder returns one order of the account.
func (s *Service) GetOrder(ctx context.Context, accountID, orderID string) (Order, error) {
	o, err := getOrder(ctx, s.pool, orderID)
	if err != nil {
		return Order{}, err
	}
	if o.TenantID != s.tenant || o.AccountID != accountID {
		return Order{}, ErrOrderNotFound
	}
	return o, nil
}

// OrdersFilter selects an account's orders; zero values mean "any".
type OrdersFilter struct {
	AccountID string
	Market    string
	Status    Status
	OpenOnly  bool
	Limit     int32
	Offset    int32
}

// ListOrders returns the account's orders, newest first.
func (s *Service) ListOrders(ctx context.Context, f OrdersFilter) ([]Order, error) {
	if f.AccountID == "" {
		return nil, fmt.Errorf("%w: account_id required", ErrInvalidRequest)
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, fmt.Errorf("%w: status %q", ErrInvalidRequest, f.Status)
	}
	limit, offset := page(f.Limit, f.Offset)
	rows, err := sqlcgen.New(s.pool).ListOrdersByAccount(ctx, sqlcgen.ListOrdersByAccountParams{
		TenantID: s.tenant, AccountID: f.AccountID, MarketSymbol: f.Market, Status: string(f.Status), OpenOnly: f.OpenOnly, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("trading: list orders: %w", err)
	}
	return ordersFromRows(rows)
}

// OrderTrades returns the trades an order took part in, oldest first.
func (s *Service) OrderTrades(ctx context.Context, accountID, orderID string) ([]Trade, error) {
	if _, err := s.GetOrder(ctx, accountID, orderID); err != nil {
		return nil, err
	}
	rows, err := sqlcgen.New(s.pool).ListTradesByOrder(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("trading: order trades: %w", err)
	}
	return tradesFromRows(rows)
}

// FillsFilter selects an account's fills.
type FillsFilter struct {
	AccountID string
	Market    string
	OrderID   string
	Limit     int32
	Offset    int32
}

// ListFills returns the account's fills (its side of each trade), newest first.
func (s *Service) ListFills(ctx context.Context, f FillsFilter) ([]Fill, error) {
	if f.AccountID == "" {
		return nil, fmt.Errorf("%w: account_id required", ErrInvalidRequest)
	}
	limit, offset := page(f.Limit, f.Offset)
	rows, err := sqlcgen.New(s.pool).ListFillsByAccount(ctx, sqlcgen.ListFillsByAccountParams{
		TenantID: s.tenant, MakerAccountID: f.AccountID, MarketSymbol: f.Market, OrderID: f.OrderID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("trading: list fills: %w", err)
	}
	trades, err := tradesFromRows(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Fill, 0, len(trades))
	for _, t := range trades {
		if fill, ok := FillOf(t, f.AccountID); ok {
			out = append(out, fill)
		}
	}
	return out, nil
}

// ListTrades returns a market's public trade history, newest first.
func (s *Service) ListTrades(ctx context.Context, market string, limit, offset int32) ([]Trade, error) {
	if _, ok := s.reg.Market(market); !ok {
		return nil, fmt.Errorf("%w: %s", ErrMarketNotFound, market)
	}
	limit, offset = page(limit, offset)
	rows, err := sqlcgen.New(s.pool).ListTradesByMarket(ctx, sqlcgen.ListTradesByMarketParams{TenantID: s.tenant, MarketSymbol: market, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("trading: list trades: %w", err)
	}
	return tradesFromRows(rows)
}

// Depth returns the aggregated order book of a market (n levels per side,
// 0 = all).
func (s *Service) Depth(ctx context.Context, market string, n int) (matching.Depth, error) {
	if _, ok := s.reg.Market(market); !ok {
		return matching.Depth{}, fmt.Errorf("%w: %s", ErrMarketNotFound, market)
	}
	return s.bus.Depth(ctx, market, n)
}

func page(limit, offset int32) (int32, int32) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
