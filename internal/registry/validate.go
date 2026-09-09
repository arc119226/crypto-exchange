package registry

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// ErrInvalid marks a registry input that violates a business rule.
var ErrInvalid = errors.New("registry: invalid input")

var (
	symbolPattern  = regexp.MustCompile(`^[A-Z0-9]{2,16}$`)
	addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

// Statuses and policies (mirrored by CHECK constraints in the schema).
const (
	AssetActive   = "active"
	AssetDisabled = "disabled"

	MarketActive     = "active"
	MarketHalted     = "halted"
	MarketCancelOnly = "cancel_only"
	MarketDelisted   = "delisted"

	STPCancelNewest = "cancel_newest"
	STPAllow        = "allow"
	STPCancelOldest = "cancel_oldest"
)

// FeeScheduleInput is the writable shape of a fee schedule.
type FeeScheduleInput struct {
	Name     string
	MakerBps int32
	TakerBps int32
}

// Validate checks the fee schedule.
func (in FeeScheduleInput) Validate() error {
	if in.Name == "" {
		return fmt.Errorf("%w: fee schedule name is empty", ErrInvalid)
	}
	for _, bps := range []int32{in.MakerBps, in.TakerBps} {
		if bps < 0 || bps > 10000 {
			return fmt.Errorf("%w: fee %d bps out of range [0,10000]", ErrInvalid, bps)
		}
	}
	return nil
}

// AssetInput is the writable shape of an asset.
type AssetInput struct {
	Symbol                string
	Name                  string
	ChainID               int64
	ContractAddress       *string
	IsNative              bool
	Scale                 int32
	DisplayScale          int32
	RequiredConfirmations int32
	MinDeposit            money.Amount
	MinWithdrawal         money.Amount
	WithdrawalFee         money.Amount
	SweepThreshold        money.Amount
	DepositEnabled        bool
	WithdrawEnabled       bool
	Status                string
}

// Validate checks the asset.
func (in AssetInput) Validate() error {
	if !symbolPattern.MatchString(in.Symbol) {
		return fmt.Errorf("%w: asset symbol %q must match %s", ErrInvalid, in.Symbol, symbolPattern)
	}
	if in.Name == "" {
		return fmt.Errorf("%w: asset %s has no name", ErrInvalid, in.Symbol)
	}
	if err := (money.Asset{Symbol: in.Symbol, Scale: in.Scale}).Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if in.DisplayScale < 0 || in.DisplayScale > in.Scale {
		return fmt.Errorf("%w: asset %s display_scale %d must be within [0,%d]", ErrInvalid, in.Symbol, in.DisplayScale, in.Scale)
	}
	if in.RequiredConfirmations <= 0 {
		return fmt.Errorf("%w: asset %s required_confirmations must be positive", ErrInvalid, in.Symbol)
	}
	if in.IsNative && in.ContractAddress != nil {
		return fmt.Errorf("%w: native asset %s must not have a contract address", ErrInvalid, in.Symbol)
	}
	if !in.IsNative && (in.ContractAddress == nil || !addressPattern.MatchString(*in.ContractAddress)) {
		return fmt.Errorf("%w: token %s needs a valid 0x contract address", ErrInvalid, in.Symbol)
	}
	for name, amt := range map[string]money.Amount{"min_deposit": in.MinDeposit, "min_withdrawal": in.MinWithdrawal, "withdrawal_fee": in.WithdrawalFee, "sweep_threshold": in.SweepThreshold} {
		if amt.IsNegative() {
			return fmt.Errorf("%w: asset %s %s must not be negative", ErrInvalid, in.Symbol, name)
		}
	}
	if in.Status != AssetActive && in.Status != AssetDisabled {
		return fmt.Errorf("%w: asset %s status %q", ErrInvalid, in.Symbol, in.Status)
	}
	return nil
}

// MarketInput is the writable shape of a market. Assets and the fee schedule
// are referenced by name and resolved inside the store.
type MarketInput struct {
	Symbol          string
	BaseSymbol      string
	QuoteSymbol     string
	PriceTick       money.Amount
	QtyStep         money.Amount
	MinNotional     money.Amount
	MaxQty          *money.Amount
	MaxSlippageBps  *int32
	FeeSchedule     string
	SelfTradePolicy string
	Status          string
}

// Validate checks everything that does not need the referenced assets.
func (in MarketInput) Validate() error {
	if !symbolPattern.MatchString(in.BaseSymbol) || !symbolPattern.MatchString(in.QuoteSymbol) {
		return fmt.Errorf("%w: market %q has invalid base/quote symbols", ErrInvalid, in.Symbol)
	}
	if in.BaseSymbol == in.QuoteSymbol {
		return fmt.Errorf("%w: market %q base and quote are the same asset", ErrInvalid, in.Symbol)
	}
	if want := in.BaseSymbol + "-" + in.QuoteSymbol; in.Symbol != want {
		return fmt.Errorf("%w: market symbol %q must be %q", ErrInvalid, in.Symbol, want)
	}
	if !in.PriceTick.IsPositive() || !in.QtyStep.IsPositive() {
		return fmt.Errorf("%w: market %s price_tick and qty_step must be positive", ErrInvalid, in.Symbol)
	}
	if in.MinNotional.IsNegative() {
		return fmt.Errorf("%w: market %s min_notional must not be negative", ErrInvalid, in.Symbol)
	}
	if in.MaxQty != nil && !in.MaxQty.IsPositive() {
		return fmt.Errorf("%w: market %s max_qty must be positive when set", ErrInvalid, in.Symbol)
	}
	if in.MaxSlippageBps != nil && (*in.MaxSlippageBps < 1 || *in.MaxSlippageBps > 10000) {
		return fmt.Errorf("%w: market %s max_slippage_bps out of range", ErrInvalid, in.Symbol)
	}
	if in.FeeSchedule == "" {
		return fmt.Errorf("%w: market %s has no fee schedule", ErrInvalid, in.Symbol)
	}
	// Only cancel_newest. The other two constants name values the column's
	// CHECK still accepts -- they exist so old rows can be read and reported --
	// but neither is a policy the engine can run: matching.MarketConfig.Validate
	// refuses cancel_oldest, and allow suppresses no self-trade at all, which
	// is wash trading rather than a policy. Accepting either here would let a
	// market be saved that no runner can restore.
	if in.SelfTradePolicy != STPCancelNewest {
		return fmt.Errorf("%w: market %s self_trade_policy %q (only %q is implemented)", ErrInvalid, in.Symbol, in.SelfTradePolicy, STPCancelNewest)
	}
	switch in.Status {
	case MarketActive, MarketHalted, MarketCancelOnly, MarketDelisted:
	default:
		return fmt.Errorf("%w: market %s status %q", ErrInvalid, in.Symbol, in.Status)
	}
	return nil
}

// ValidatePrecision enforces docs/plan-v1.0.md §6.5:
// scale(qty_step) + scale(price_tick) ≤ quote.scale, so price × qty never
// needs rounding, and qty_step must fit the base asset scale.
func ValidatePrecision(priceTick, qtyStep money.Amount, baseScale, quoteScale int32) error {
	if qtyStep.Scale() > baseScale {
		return fmt.Errorf("%w: qty_step %s has more decimals than the base asset scale %d", ErrInvalid, qtyStep, baseScale)
	}
	if sum := priceTick.Scale() + qtyStep.Scale(); sum > quoteScale {
		return fmt.Errorf("%w: scale(price_tick)+scale(qty_step) = %d exceeds quote asset scale %d", ErrInvalid, sum, quoteScale)
	}
	return nil
}

// ValidateWithdrawalLimit checks one limit row (docs/plan-v1.0.md §6.4.2).
func ValidateWithdrawalLimit(in WithdrawalLimitInput) error {
	if in.Asset == "" {
		return fmt.Errorf("%w: withdrawal limit asset is required", ErrInvalid)
	}
	if in.KYCLevel < 0 || in.KYCLevel > 2 {
		return fmt.Errorf("%w: withdrawal limit %s kyc_level %d is outside 0..2", ErrInvalid, in.Asset, in.KYCLevel)
	}
	if in.AutoApproveLimit.IsNegative() || in.DailyLimit.IsNegative() {
		return fmt.Errorf("%w: withdrawal limit %s amounts must not be negative", ErrInvalid, in.Asset)
	}
	// A per-request ceiling above the daily one can never bind: the daily
	// check would reject first. Saying so here beats a limit that silently
	// does nothing.
	if in.AutoApproveLimit.Cmp(in.DailyLimit) > 0 {
		return fmt.Errorf("%w: withdrawal limit %s auto_approve_limit %s exceeds daily_limit %s",
			ErrInvalid, in.Asset, in.AutoApproveLimit, in.DailyLimit)
	}
	return nil
}
