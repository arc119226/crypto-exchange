package ledger

import (
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// SettleParams describes one trade to settle. Quantities come straight from
// matching.Trade; the ledger adds fees and the buyer's price-improvement
// release (docs/plan-v1.0.md §6.1.4 b).
type SettleParams struct {
	TradeID        string
	IdempotencyKey string // default settle:trade:{TradeID}
	CorrelationID  string

	BuyerAccountID  string
	SellerAccountID string
	BuyerIsTaker    bool

	BaseAsset  string
	QuoteAsset string
	Price      money.Amount
	Qty        money.Amount
	QuoteQty   money.Amount // must equal Price × Qty

	// BuyerLimitPrice is the price the buyer's quote hold was computed with
	// (limit orders). When it exceeds Price, (limit − price) × qty of the
	// hold is released back to available in the same entry. Zero for market
	// buys, whose leftover budget is released when the order completes.
	BuyerLimitPrice money.Amount

	Fees FeeParams
}

// SettleResult reports what the settlement charged and released.
type SettleResult struct {
	Entry     JournalEntry
	BuyerFee  money.Amount // in base (the asset the buyer receives)
	SellerFee money.Amount // in quote
	MakerFee  money.Amount
	TakerFee  money.Amount
	Release   money.Amount // quote released to the buyer for price improvement
}

// BuildSettleEntry is the pure part of Settle: it computes fees and the
// price-improvement release and returns the balanced entry. feeRevenue is
// the fee_revenue house account id.
func BuildSettleEntry(p SettleParams, feeRevenue string) (Entry, SettleResult, error) {
	var res SettleResult
	if p.TradeID == "" || p.BuyerAccountID == "" || p.SellerAccountID == "" || p.BaseAsset == "" || p.QuoteAsset == "" || feeRevenue == "" {
		return Entry{}, res, fmt.Errorf("%w: missing ids", ErrInvalidSettlement)
	}
	if p.BaseAsset == p.QuoteAsset {
		return Entry{}, res, fmt.Errorf("%w: base and quote asset are equal", ErrInvalidSettlement)
	}
	if !p.Price.IsPositive() || !p.Qty.IsPositive() || !p.QuoteQty.IsPositive() {
		return Entry{}, res, fmt.Errorf("%w: price, qty and quote qty must be positive", ErrInvalidSettlement)
	}
	if !p.Price.Mul(p.Qty).Equal(p.QuoteQty) {
		return Entry{}, res, fmt.Errorf("%w: quote qty %s != price %s × qty %s", ErrInvalidSettlement, p.QuoteQty, p.Price, p.Qty)
	}
	if err := p.Fees.Validate(); err != nil {
		return Entry{}, res, err
	}
	if p.Qty.Scale() > p.Fees.BaseScale || p.QuoteQty.Scale() > p.Fees.QuoteScale {
		return Entry{}, res, fmt.Errorf("%w: amounts exceed asset scales", ErrInvalidSettlement)
	}
	if p.BuyerLimitPrice.IsNegative() || (p.BuyerLimitPrice.IsPositive() && p.BuyerLimitPrice.Cmp(p.Price) < 0) {
		return Entry{}, res, fmt.Errorf("%w: buyer limit %s below trade price %s", ErrInvalidSettlement, p.BuyerLimitPrice, p.Price)
	}

	buyerBps, sellerBps := p.Fees.MakerBps, p.Fees.TakerBps
	if p.BuyerIsTaker {
		buyerBps, sellerBps = p.Fees.TakerBps, p.Fees.MakerBps
	}
	buyerFee, err := ComputeFee(p.Qty, buyerBps, p.Fees.BaseScale) // buyer receives base
	if err != nil {
		return Entry{}, res, err
	}
	sellerFee, err := ComputeFee(p.QuoteQty, sellerBps, p.Fees.QuoteScale) // seller receives quote
	if err != nil {
		return Entry{}, res, err
	}
	if buyerFee.Cmp(p.Qty) >= 0 || sellerFee.Cmp(p.QuoteQty) >= 0 {
		return Entry{}, res, fmt.Errorf("%w: fee would consume the whole fill", ErrInvalidSettlement)
	}
	release := money.Zero
	if p.BuyerLimitPrice.IsPositive() && p.BuyerLimitPrice.Cmp(p.Price) > 0 {
		release = p.BuyerLimitPrice.Sub(p.Price).Mul(p.Qty)
	}

	key := p.IdempotencyKey
	if key == "" {
		key = "settle:trade:" + p.TradeID
	}
	postings := []Posting{
		// quote leg: buyer's hold pays the seller (net of the seller's fee)
		{AccountID: p.BuyerAccountID, Asset: p.QuoteAsset, Bucket: BucketHold, Direction: Debit, Amount: p.QuoteQty},
		{AccountID: p.SellerAccountID, Asset: p.QuoteAsset, Bucket: BucketAvailable, Direction: Credit, Amount: p.QuoteQty.Sub(sellerFee)},
	}
	if sellerFee.IsPositive() {
		postings = append(postings, Posting{AccountID: feeRevenue, Asset: p.QuoteAsset, Bucket: BucketHouse, Direction: Credit, Amount: sellerFee})
	}
	// base leg: seller's hold pays the buyer (net of the buyer's fee)
	postings = append(postings,
		Posting{AccountID: p.SellerAccountID, Asset: p.BaseAsset, Bucket: BucketHold, Direction: Debit, Amount: p.Qty},
		Posting{AccountID: p.BuyerAccountID, Asset: p.BaseAsset, Bucket: BucketAvailable, Direction: Credit, Amount: p.Qty.Sub(buyerFee)},
	)
	if buyerFee.IsPositive() {
		postings = append(postings, Posting{AccountID: feeRevenue, Asset: p.BaseAsset, Bucket: BucketHouse, Direction: Credit, Amount: buyerFee})
	}
	if release.IsPositive() {
		postings = append(postings,
			Posting{AccountID: p.BuyerAccountID, Asset: p.QuoteAsset, Bucket: BucketHold, Direction: Debit, Amount: release},
			Posting{AccountID: p.BuyerAccountID, Asset: p.QuoteAsset, Bucket: BucketAvailable, Direction: Credit, Amount: release},
		)
	}
	entry := Entry{IdempotencyKey: key, Kind: KindSettle, RefType: "trade", RefID: p.TradeID, CorrelationID: p.CorrelationID, Postings: postings}
	if err := entry.Validate(); err != nil {
		return Entry{}, res, err
	}
	res.BuyerFee, res.SellerFee, res.Release = buyerFee, sellerFee, release
	if p.BuyerIsTaker {
		res.TakerFee, res.MakerFee = buyerFee, sellerFee
	} else {
		res.MakerFee, res.TakerFee = buyerFee, sellerFee
	}
	return entry, res, nil
}
