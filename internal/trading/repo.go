package trading

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/trading/sqlcgen"
)

func orderFromRow(r sqlcgen.TradingOrder) (Order, error) {
	o := Order{
		ID: r.ID, TenantID: r.TenantID, AccountID: r.AccountID, MarketID: r.MarketID, MarketSymbol: r.MarketSymbol,
		ClientOrderID: r.ClientOrderID, HoldAsset: r.HoldAsset, Status: Status(r.Status),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if err := o.Side.UnmarshalText([]byte(r.Side)); err != nil {
		return Order{}, err
	}
	if err := o.Type.UnmarshalText([]byte(r.Type)); err != nil {
		return Order{}, err
	}
	if err := o.TimeInForce.UnmarshalText([]byte(r.TimeInForce)); err != nil {
		return Order{}, err
	}
	var err error
	if o.Price, err = pg.NullableAmountFromNumeric(r.Price); err != nil {
		return Order{}, err
	}
	if o.Qty, err = pg.NullableAmountFromNumeric(r.Qty); err != nil {
		return Order{}, err
	}
	if o.QuoteQty, err = pg.NullableAmountFromNumeric(r.QuoteQty); err != nil {
		return Order{}, err
	}
	if o.FilledQty, err = pg.AmountFromNumeric(r.FilledQty); err != nil {
		return Order{}, err
	}
	if o.FilledQuote, err = pg.AmountFromNumeric(r.FilledQuote); err != nil {
		return Order{}, err
	}
	if o.RemainingQty, err = pg.AmountFromNumeric(r.RemainingQty); err != nil {
		return Order{}, err
	}
	if o.HoldAmount, err = pg.AmountFromNumeric(r.HoldAmount); err != nil {
		return Order{}, err
	}
	if o.HoldRemaining, err = pg.AmountFromNumeric(r.HoldRemaining); err != nil {
		return Order{}, err
	}
	if r.RejectReason != nil {
		o.RejectReason = RejectReason(*r.RejectReason)
	}
	if r.CancelReason != nil {
		o.CancelReason = matching.CancelReason(*r.CancelReason)
	}
	if r.Seq != nil {
		s := uint64(*r.Seq) //nolint:gosec // stored from a uint64
		o.Seq = &s
	}
	if r.CorrelationID != nil {
		o.CorrelationID = *r.CorrelationID
	}
	return o, nil
}

func tradeFromRow(r sqlcgen.TradingTrade) (Trade, error) {
	t := Trade{
		ID: r.ID, TenantID: r.TenantID, MarketID: r.MarketID, MarketSymbol: r.MarketSymbol,
		Seq: uint64(r.Seq), Index: int(r.Idx), //nolint:gosec // stored from a uint64 / small int
		MakerOrderID: r.MakerOrderID, TakerOrderID: r.TakerOrderID, MakerAccountID: r.MakerAccountID, TakerAccountID: r.TakerAccountID,
		MakerFeeAsset: r.MakerFeeAsset, TakerFeeAsset: r.TakerFeeAsset, CreatedAt: r.CreatedAt,
	}
	if err := t.TakerSide.UnmarshalText([]byte(r.TakerSide)); err != nil {
		return Trade{}, err
	}
	var err error
	if t.Price, err = pg.AmountFromNumeric(r.Price); err != nil {
		return Trade{}, err
	}
	if t.Qty, err = pg.AmountFromNumeric(r.Qty); err != nil {
		return Trade{}, err
	}
	if t.QuoteQty, err = pg.AmountFromNumeric(r.QuoteQty); err != nil {
		return Trade{}, err
	}
	if t.MakerFee, err = pg.AmountFromNumeric(r.MakerFee); err != nil {
		return Trade{}, err
	}
	if t.TakerFee, err = pg.AmountFromNumeric(r.TakerFee); err != nil {
		return Trade{}, err
	}
	return t, nil
}

func ordersFromRows(rows []sqlcgen.TradingOrder) ([]Order, error) {
	out := make([]Order, 0, len(rows))
	for _, r := range rows {
		o, err := orderFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("trading: order %s: %w", r.ID, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func tradesFromRows(rows []sqlcgen.TradingTrade) ([]Trade, error) {
	out := make([]Trade, 0, len(rows))
	for _, r := range rows {
		t, err := tradeFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("trading: trade %s: %w", r.ID, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// insertParams turns an Order into the INSERT parameters.
func insertParams(o Order) sqlcgen.InsertOrderParams {
	var seq *int64
	if o.Seq != nil {
		v := int64(*o.Seq) //nolint:gosec // engine seq
		seq = &v
	}
	return sqlcgen.InsertOrderParams{
		ID: o.ID, TenantID: o.TenantID, AccountID: o.AccountID, MarketID: o.MarketID, MarketSymbol: o.MarketSymbol,
		ClientOrderID: o.ClientOrderID, Side: o.Side.String(), Type: o.Type.String(), TimeInForce: o.TimeInForce.String(),
		Price: pg.NullableNumericFromAmount(o.Price), Qty: pg.NullableNumericFromAmount(o.Qty), QuoteQty: pg.NullableNumericFromAmount(o.QuoteQty),
		FilledQty: pg.NumericFromAmount(o.FilledQty), FilledQuote: pg.NumericFromAmount(o.FilledQuote), RemainingQty: pg.NumericFromAmount(o.RemainingQty),
		HoldAsset: o.HoldAsset, HoldAmount: pg.NumericFromAmount(o.HoldAmount), HoldRemaining: pg.NumericFromAmount(o.HoldRemaining),
		Status: string(o.Status), RejectReason: optStr(string(o.RejectReason)), CancelReason: optStr(string(o.CancelReason)),
		Seq: seq, CorrelationID: optStr(o.CorrelationID), CreatedAt: o.CreatedAt,
	}
}

func progressParams(o Order) sqlcgen.UpdateOrderProgressParams {
	return sqlcgen.UpdateOrderProgressParams{
		ID: o.ID, FilledQty: pg.NumericFromAmount(o.FilledQty), FilledQuote: pg.NumericFromAmount(o.FilledQuote),
		RemainingQty: pg.NumericFromAmount(o.RemainingQty), HoldRemaining: pg.NumericFromAmount(o.HoldRemaining),
		Status: string(o.Status), CancelReason: optStr(string(o.CancelReason)),
	}
}

func tradeParams(t Trade) sqlcgen.InsertTradeParams {
	return sqlcgen.InsertTradeParams{
		ID: t.ID, TenantID: t.TenantID, MarketID: t.MarketID, MarketSymbol: t.MarketSymbol,
		Seq: int64(t.Seq), Idx: int32(t.Index), //nolint:gosec // engine seq / trade index
		MakerOrderID: t.MakerOrderID, TakerOrderID: t.TakerOrderID, MakerAccountID: t.MakerAccountID, TakerAccountID: t.TakerAccountID,
		TakerSide: t.TakerSide.String(), Price: pg.NumericFromAmount(t.Price), Qty: pg.NumericFromAmount(t.Qty), QuoteQty: pg.NumericFromAmount(t.QuoteQty),
		MakerFee: pg.NumericFromAmount(t.MakerFee), MakerFeeAsset: t.MakerFeeAsset, TakerFee: pg.NumericFromAmount(t.TakerFee), TakerFeeAsset: t.TakerFeeAsset,
		CreatedAt: t.CreatedAt,
	}
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// getOrder loads one order; ErrOrderNotFound when it does not exist.
func getOrder(ctx context.Context, db sqlcgen.DBTX, id string) (Order, error) {
	row, err := sqlcgen.New(db).GetOrder(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrOrderNotFound
		}
		return Order{}, fmt.Errorf("trading: get order: %w", err)
	}
	return orderFromRow(row)
}

// getOrderByClientID loads the order an account placed under a client id.
func getOrderByClientID(ctx context.Context, db sqlcgen.DBTX, tenant, account, clientID string) (Order, error) {
	row, err := sqlcgen.New(db).GetOrderByClientID(ctx, sqlcgen.GetOrderByClientIDParams{TenantID: tenant, AccountID: account, ClientOrderID: clientID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrOrderNotFound
		}
		return Order{}, fmt.Errorf("trading: get order by client id: %w", err)
	}
	return orderFromRow(row)
}
