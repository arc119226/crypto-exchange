package matching

import "github.com/arc119226/crypto-exchange/internal/money"

// ValidateNewOrder applies the static rules of docs/plan-v1.0.md §6.3 /
// §6.5 to an order and returns the reject reason, or "" when the order is
// well-formed. internal/trading runs the same check before holding funds;
// the book runs it again so Apply is total (panic-free) for any input.
//
// Rules: ids present; side/type known; limit → price and qty positive,
// no quote qty; market sell → qty positive, no price/quote; market buy →
// quote qty positive, no price/qty; price is a tick multiple; qty is a step
// multiple; amounts fit the base/quote scales; price × qty (limit) or
// quote qty (market buy) ≥ min_notional; qty ≤ max_qty when configured.
func ValidateNewOrder(cfg MarketConfig, o NewOrder) RejectReason {
	if o.OrderID == "" || o.AccountID == "" || (o.Side != Buy && o.Side != Sell) {
		return RejectInvalidOrder
	}
	switch o.Type {
	case Limit:
		if o.TimeInForce != GTC && o.TimeInForce != IOC && o.TimeInForce != 0 {
			return RejectInvalidOrder
		}
		if !o.Price.IsPositive() || !o.Qty.IsPositive() || !o.QuoteQty.IsZero() {
			return RejectInvalidOrder
		}
		if o.Price.Scale() > cfg.QuoteScale || !o.Price.IsMultipleOf(cfg.PriceTick) {
			return RejectInvalidPriceTick
		}
		if r := validateBaseQty(cfg, o.Qty); r != "" {
			return r
		}
		if o.Price.Mul(o.Qty).Cmp(cfg.MinNotional) < 0 {
			return RejectBelowMinNotional
		}
	case Market:
		if o.TimeInForce != IOC && o.TimeInForce != 0 {
			return RejectInvalidOrder
		}
		if !o.Price.IsZero() {
			return RejectInvalidOrder
		}
		if o.Side == Sell {
			if !o.Qty.IsPositive() || !o.QuoteQty.IsZero() {
				return RejectInvalidOrder
			}
			if r := validateBaseQty(cfg, o.Qty); r != "" {
				return r
			}
		} else {
			if !o.QuoteQty.IsPositive() || !o.Qty.IsZero() {
				return RejectInvalidOrder
			}
			if o.QuoteQty.Scale() > cfg.QuoteScale {
				return RejectInvalidOrder
			}
			if o.QuoteQty.Cmp(cfg.MinNotional) < 0 {
				return RejectBelowMinNotional
			}
		}
	default:
		return RejectInvalidOrder
	}
	return ""
}

func validateBaseQty(cfg MarketConfig, qty money.Amount) RejectReason {
	if qty.Scale() > cfg.BaseScale || !qty.IsMultipleOf(cfg.QtyStep) {
		return RejectInvalidQtyStep
	}
	if cfg.MaxQty != nil && qty.Cmp(*cfg.MaxQty) > 0 {
		return RejectAboveMaxQty
	}
	return ""
}

// floorToStep returns the largest multiple of step that is <= x (x, step > 0).
func floorToStep(x, step money.Amount) (money.Amount, error) {
	n, err := x.DivRoundDown(step, 0)
	if err != nil {
		return money.Zero, err
	}
	return n.Mul(step), nil
}
