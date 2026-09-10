// Package registry owns the asset / market / fee-schedule / withdrawal-limit
// configuration (docs/plan-v1.0.md §6.6). Postgres is the source of truth;
// the admin role edits it and the engine loads it on start (and on reload).
package registry

import (
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry/sqlcgen"
)

// Asset is a currency the exchange lists.
type Asset struct {
	ID                    string
	TenantID              string
	Symbol                string
	Name                  string
	ChainID               int64
	ContractAddress       *string // nil for the native coin
	IsNative              bool
	Scale                 int32
	DisplayScale          int32
	RequiredConfirmations int32
	MinDeposit            money.Amount
	MinWithdrawal         money.Amount
	// WithdrawalFee is the flat part of the withdrawal fee and
	// WithdrawalFeeBps the proportional part; Fees computes what a given
	// amount is charged (§23.3). DepositFeeBps is the deposit fee, which is
	// deducted from what arrives rather than added to it (§23.4) and is 0 in
	// the seed because a deposit is the inlet -- charging for it turns money
	// away at the door.
	WithdrawalFee    money.Amount
	WithdrawalFeeBps int32
	DepositFeeBps    int32
	SweepThreshold   money.Amount
	DepositEnabled   bool
	WithdrawEnabled  bool
	Status           string
	Version          int32
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Money returns the money.Asset view (symbol + scale).
func (a Asset) Money() money.Asset { return money.Asset{Symbol: a.Symbol, Scale: a.Scale} }

// FeeSchedule holds maker/taker fees in basis points.
type FeeSchedule struct {
	ID            string
	TenantID      string
	Name          string
	MakerBps      int32
	TakerBps      int32
	EffectiveFrom time.Time
	Version       int32
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Market is a trading pair with its precision and fee configuration.
type Market struct {
	ID              string
	TenantID        string
	Symbol          string
	BaseAssetID     string
	QuoteAssetID    string
	BaseSymbol      string
	QuoteSymbol     string
	BaseScale       int32
	QuoteScale      int32
	PriceTick       money.Amount
	QtyStep         money.Amount
	MinNotional     money.Amount
	MaxQty          *money.Amount
	MaxSlippageBps  *int32
	FeeScheduleID   string
	FeeScheduleName string
	MakerBps        int32
	TakerBps        int32
	SelfTradePolicy string
	Status          string
	Version         int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func assetFromRow(r sqlcgen.RegistryAsset) (Asset, error) {
	minDeposit, err := pg.AmountFromNumeric(r.MinDeposit)
	if err != nil {
		return Asset{}, err
	}
	minWithdrawal, err := pg.AmountFromNumeric(r.MinWithdrawal)
	if err != nil {
		return Asset{}, err
	}
	withdrawalFee, err := pg.AmountFromNumeric(r.WithdrawalFee)
	if err != nil {
		return Asset{}, err
	}
	sweep, err := pg.AmountFromNumeric(r.SweepThreshold)
	if err != nil {
		return Asset{}, err
	}
	return Asset{
		ID: r.ID, TenantID: r.TenantID, Symbol: r.Symbol, Name: r.Name, ChainID: r.ChainID,
		ContractAddress: r.ContractAddress, IsNative: r.IsNative,
		Scale: int32(r.Scale), DisplayScale: int32(r.DisplayScale), RequiredConfirmations: r.RequiredConfirmations,
		MinDeposit: minDeposit, MinWithdrawal: minWithdrawal, WithdrawalFee: withdrawalFee,
		WithdrawalFeeBps: r.WithdrawalFeeBps, DepositFeeBps: r.DepositFeeBps, SweepThreshold: sweep,
		DepositEnabled: r.DepositEnabled, WithdrawEnabled: r.WithdrawEnabled, Status: r.Status,
		Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

func feeScheduleFromRow(r sqlcgen.RegistryFeeSchedule) FeeSchedule {
	return FeeSchedule{
		ID: r.ID, TenantID: r.TenantID, Name: r.Name, MakerBps: r.MakerBps, TakerBps: r.TakerBps,
		EffectiveFrom: r.EffectiveFrom, Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func marketFromRow(r sqlcgen.ListMarketsRow) (Market, error) {
	tick, err := pg.AmountFromNumeric(r.PriceTick)
	if err != nil {
		return Market{}, err
	}
	step, err := pg.AmountFromNumeric(r.QtyStep)
	if err != nil {
		return Market{}, err
	}
	minNotional, err := pg.AmountFromNumeric(r.MinNotional)
	if err != nil {
		return Market{}, err
	}
	maxQty, err := pg.NullableAmountFromNumeric(r.MaxQty)
	if err != nil {
		return Market{}, err
	}
	return Market{
		ID: r.ID, TenantID: r.TenantID, Symbol: r.Symbol,
		BaseAssetID: r.BaseAssetID, QuoteAssetID: r.QuoteAssetID,
		BaseSymbol: r.BaseSymbol, QuoteSymbol: r.QuoteSymbol,
		BaseScale: int32(r.BaseScale), QuoteScale: int32(r.QuoteScale),
		PriceTick: tick, QtyStep: step, MinNotional: minNotional, MaxQty: maxQty, MaxSlippageBps: r.MaxSlippageBps,
		FeeScheduleID: r.FeeScheduleID, FeeScheduleName: r.FeeScheduleName, MakerBps: r.MakerBps, TakerBps: r.TakerBps,
		SelfTradePolicy: r.SelfTradePolicy, Status: r.Status,
		Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

// WithdrawalLimit is one row of registry.withdrawal_limits: what an account at
// a given KYC level may withdraw of one asset without a person looking at it
// (docs/plan-v1.0.md §6.4.2). The table has existed since migration 0002; the
// withdrawal policy is the first thing to read it.
type WithdrawalLimit struct {
	TenantID string
	AssetID  string
	Asset    string
	KYCLevel int16
	// AutoApproveLimit is the largest single withdrawal that skips review.
	AutoApproveLimit money.Amount
	// DailyLimit caps the rolling 24 h total; over it the request is rejected
	// rather than queued, because waiting will not make it fit.
	DailyLimit money.Amount
	// RequireManualReview sends every withdrawal of this asset at this level
	// to the queue whatever the amounts say.
	RequireManualReview bool
	Version             int32
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// WithdrawalLimitInput is the admin-facing upsert shape.
type WithdrawalLimitInput struct {
	Asset               string
	KYCLevel            int16
	AutoApproveLimit    money.Amount
	DailyLimit          money.Amount
	RequireManualReview bool
}

func withdrawalLimitFromRow(r sqlcgen.ListWithdrawalLimitsRow) (WithdrawalLimit, error) {
	auto, err := pg.AmountFromNumeric(r.AutoApproveLimit)
	if err != nil {
		return WithdrawalLimit{}, err
	}
	daily, err := pg.AmountFromNumeric(r.DailyLimit)
	if err != nil {
		return WithdrawalLimit{}, err
	}
	return WithdrawalLimit{
		TenantID: r.TenantID, AssetID: r.AssetID, Asset: r.AssetSymbol, KYCLevel: r.KycLevel,
		AutoApproveLimit: auto, DailyLimit: daily, RequireManualReview: r.RequireManualReview,
		Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}
