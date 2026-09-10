package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry/sqlcgen"
)

// ErrNotFound is returned when a symbol or name does not exist for the tenant.
var ErrNotFound = errors.New("registry: not found")

// Reader is the read-only view consumed by the API and the engine.
type Reader interface {
	ListAssets(ctx context.Context, tenantID string) ([]Asset, error)
	GetAsset(ctx context.Context, tenantID, symbol string) (Asset, error)
	ListMarkets(ctx context.Context, tenantID string) ([]Market, error)
	GetMarket(ctx context.Context, tenantID, symbol string) (Market, error)
}

// Store is the Postgres-backed registry.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlcgen.New(pool)}
}

// Pool exposes the underlying pool for transactions (seed).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ListAssets returns every asset of the tenant ordered by symbol.
func (s *Store) ListAssets(ctx context.Context, tenantID string) ([]Asset, error) {
	rows, err := s.q.ListAssets(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("registry: list assets: %w", err)
	}
	out := make([]Asset, 0, len(rows))
	for _, r := range rows {
		a, err := assetFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("registry: asset %s: %w", r.Symbol, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// GetAsset returns one asset or ErrNotFound.
func (s *Store) GetAsset(ctx context.Context, tenantID, symbol string) (Asset, error) {
	r, err := s.q.GetAssetBySymbol(ctx, sqlcgen.GetAssetBySymbolParams{TenantID: tenantID, Symbol: symbol})
	if err != nil {
		return Asset{}, mapErr("asset "+symbol, err)
	}
	return assetFromRow(r)
}

// ListMarkets returns every market of the tenant ordered by symbol.
func (s *Store) ListMarkets(ctx context.Context, tenantID string) ([]Market, error) {
	rows, err := s.q.ListMarkets(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("registry: list markets: %w", err)
	}
	out := make([]Market, 0, len(rows))
	for _, r := range rows {
		m, err := marketFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("registry: market %s: %w", r.Symbol, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMarket returns one market or ErrNotFound.
func (s *Store) GetMarket(ctx context.Context, tenantID, symbol string) (Market, error) {
	r, err := s.q.GetMarketBySymbol(ctx, sqlcgen.GetMarketBySymbolParams{TenantID: tenantID, Symbol: symbol})
	if err != nil {
		return Market{}, mapErr("market "+symbol, err)
	}
	return marketFromRow(sqlcgen.ListMarketsRow(r))
}

// GetFeeSchedule returns one fee schedule or ErrNotFound.
func (s *Store) GetFeeSchedule(ctx context.Context, tenantID, name string) (FeeSchedule, error) {
	r, err := s.q.GetFeeScheduleByName(ctx, sqlcgen.GetFeeScheduleByNameParams{TenantID: tenantID, Name: name})
	if err != nil {
		return FeeSchedule{}, mapErr("fee schedule "+name, err)
	}
	return feeScheduleFromRow(r), nil
}

// UpsertFeeSchedule inserts or updates a fee schedule inside tx.
func (s *Store) UpsertFeeSchedule(ctx context.Context, tx pgx.Tx, tenantID string, in FeeScheduleInput) (FeeSchedule, error) {
	if err := in.Validate(); err != nil {
		return FeeSchedule{}, err
	}
	q := s.q.WithTx(tx)
	if err := q.UpsertFeeSchedule(ctx, sqlcgen.UpsertFeeScheduleParams{TenantID: tenantID, Name: in.Name, MakerBps: in.MakerBps, TakerBps: in.TakerBps}); err != nil {
		return FeeSchedule{}, fmt.Errorf("registry: upsert fee schedule %s: %w", in.Name, err)
	}
	r, err := q.GetFeeScheduleByName(ctx, sqlcgen.GetFeeScheduleByNameParams{TenantID: tenantID, Name: in.Name})
	if err != nil {
		return FeeSchedule{}, mapErr("fee schedule "+in.Name, err)
	}
	return feeScheduleFromRow(r), nil
}

// UpsertAsset inserts or updates an asset inside tx.
func (s *Store) UpsertAsset(ctx context.Context, tx pgx.Tx, tenantID string, in AssetInput) (Asset, error) {
	if err := in.Validate(); err != nil {
		return Asset{}, err
	}
	q := s.q.WithTx(tx)
	err := q.UpsertAsset(ctx, sqlcgen.UpsertAssetParams{
		TenantID: tenantID, Symbol: in.Symbol, Name: in.Name, ChainID: in.ChainID,
		ContractAddress: in.ContractAddress, IsNative: in.IsNative,
		Scale: int16(in.Scale), DisplayScale: int16(in.DisplayScale), //nolint:gosec // validated to [0,18] above
		RequiredConfirmations: in.RequiredConfirmations,
		MinDeposit:            pg.NumericFromAmount(in.MinDeposit),
		MinWithdrawal:         pg.NumericFromAmount(in.MinWithdrawal),
		WithdrawalFee:         pg.NumericFromAmount(in.WithdrawalFee),
		WithdrawalFeeBps:      in.WithdrawalFeeBps,
		DepositFeeBps:         in.DepositFeeBps,
		SweepThreshold:        pg.NumericFromAmount(in.SweepThreshold),
		DepositEnabled:        in.DepositEnabled, WithdrawEnabled: in.WithdrawEnabled, Status: in.Status,
	})
	if err != nil {
		return Asset{}, fmt.Errorf("registry: upsert asset %s: %w", in.Symbol, err)
	}
	r, err := q.GetAssetBySymbol(ctx, sqlcgen.GetAssetBySymbolParams{TenantID: tenantID, Symbol: in.Symbol})
	if err != nil {
		return Asset{}, mapErr("asset "+in.Symbol, err)
	}
	return assetFromRow(r)
}

// UpsertMarket inserts or updates a market inside tx, resolving the base and
// quote assets and the fee schedule by name and enforcing the precision rule.
func (s *Store) UpsertMarket(ctx context.Context, tx pgx.Tx, tenantID string, in MarketInput) (Market, error) {
	if err := in.Validate(); err != nil {
		return Market{}, err
	}
	q := s.q.WithTx(tx)
	base, err := q.GetAssetBySymbol(ctx, sqlcgen.GetAssetBySymbolParams{TenantID: tenantID, Symbol: in.BaseSymbol})
	if err != nil {
		return Market{}, mapErr("base asset "+in.BaseSymbol, err)
	}
	quote, err := q.GetAssetBySymbol(ctx, sqlcgen.GetAssetBySymbolParams{TenantID: tenantID, Symbol: in.QuoteSymbol})
	if err != nil {
		return Market{}, mapErr("quote asset "+in.QuoteSymbol, err)
	}
	fee, err := q.GetFeeScheduleByName(ctx, sqlcgen.GetFeeScheduleByNameParams{TenantID: tenantID, Name: in.FeeSchedule})
	if err != nil {
		return Market{}, mapErr("fee schedule "+in.FeeSchedule, err)
	}
	if err := ValidatePrecision(in.PriceTick, in.QtyStep, int32(base.Scale), int32(quote.Scale)); err != nil {
		return Market{}, fmt.Errorf("market %s: %w", in.Symbol, err)
	}
	err = q.UpsertMarket(ctx, sqlcgen.UpsertMarketParams{
		TenantID: tenantID, Symbol: in.Symbol, BaseAssetID: base.ID, QuoteAssetID: quote.ID,
		PriceTick: pg.NumericFromAmount(in.PriceTick), QtyStep: pg.NumericFromAmount(in.QtyStep),
		MinNotional: pg.NumericFromAmount(in.MinNotional), MaxQty: pg.NullableNumericFromAmount(in.MaxQty),
		MaxSlippageBps: in.MaxSlippageBps, FeeScheduleID: fee.ID,
		SelfTradePolicy: in.SelfTradePolicy, Status: in.Status,
	})
	if err != nil {
		return Market{}, fmt.Errorf("registry: upsert market %s: %w", in.Symbol, err)
	}
	r, err := q.GetMarketBySymbol(ctx, sqlcgen.GetMarketBySymbolParams{TenantID: tenantID, Symbol: in.Symbol})
	if err != nil {
		return Market{}, mapErr("market "+in.Symbol, err)
	}
	return marketFromRow(sqlcgen.ListMarketsRow(r))
}

func mapErr(what string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return fmt.Errorf("registry: %s: %w", what, err)
}

// ValidMarketStatus reports whether s is one of the four market statuses.
func ValidMarketStatus(s string) bool {
	switch s {
	case MarketActive, MarketHalted, MarketCancelOnly, MarketDelisted:
		return true
	}
	return false
}

// SetMarketStatus moves a market between active, halted, cancel_only and
// delisted (docs/plan-v1.0.md §6.6). It reports whether the status actually
// changed: setting the status a market already has is a no-op, so the caller
// writes neither an audit record nor an event for it.
func (s *Store) SetMarketStatus(ctx context.Context, tx pgx.Tx, tenantID, symbol, status string) (Market, bool, error) {
	if !ValidMarketStatus(status) {
		return Market{}, false, fmt.Errorf("%w: market status %q", ErrInvalid, status)
	}
	q := s.q.WithTx(tx)
	before, err := q.GetMarketBySymbol(ctx, sqlcgen.GetMarketBySymbolParams{TenantID: tenantID, Symbol: symbol})
	if err != nil {
		return Market{}, false, mapErr("market "+symbol, err)
	}
	if before.Status == status {
		m, err := marketFromRow(sqlcgen.ListMarketsRow(before))
		return m, false, err
	}
	if err := q.SetMarketStatus(ctx, sqlcgen.SetMarketStatusParams{TenantID: tenantID, Symbol: symbol, Status: status}); err != nil {
		return Market{}, false, fmt.Errorf("registry: set market %s status: %w", symbol, err)
	}
	after, err := q.GetMarketBySymbol(ctx, sqlcgen.GetMarketBySymbolParams{TenantID: tenantID, Symbol: symbol})
	if err != nil {
		return Market{}, false, mapErr("market "+symbol, err)
	}
	m, err := marketFromRow(sqlcgen.ListMarketsRow(after))
	return m, true, err
}

// ListWithdrawalLimits returns every configured limit of the tenant.
func (s *Store) ListWithdrawalLimits(ctx context.Context, tenantID string) ([]WithdrawalLimit, error) {
	rows, err := s.q.ListWithdrawalLimits(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("registry: list withdrawal limits: %w", err)
	}
	out := make([]WithdrawalLimit, 0, len(rows))
	for _, r := range rows {
		l, err := withdrawalLimitFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("registry: withdrawal limit %s/%d: %w", r.AssetSymbol, r.KycLevel, err)
		}
		out = append(out, l)
	}
	return out, nil
}

// GetWithdrawalLimit returns the limit for one asset at one KYC level, or
// ErrNotFound. A missing row is not "no limit": the caller must decide, and
// the withdrawal policy treats it as review-everything.
func (s *Store) GetWithdrawalLimit(ctx context.Context, tenantID, symbol string, kycLevel int16) (WithdrawalLimit, error) {
	r, err := s.q.GetWithdrawalLimit(ctx, sqlcgen.GetWithdrawalLimitParams{TenantID: tenantID, Symbol: symbol, KycLevel: kycLevel})
	if err != nil {
		return WithdrawalLimit{}, mapErr(fmt.Sprintf("withdrawal limit %s/%d", symbol, kycLevel), err)
	}
	return withdrawalLimitFromRow(sqlcgen.ListWithdrawalLimitsRow(r))
}

// UpsertWithdrawalLimit writes one limit. Like the other upserts it only bumps
// the version when a value actually changed, so re-seeding is a no-op.
func (s *Store) UpsertWithdrawalLimit(ctx context.Context, tx pgx.Tx, tenantID string, in WithdrawalLimitInput) (WithdrawalLimit, error) {
	if err := ValidateWithdrawalLimit(in); err != nil {
		return WithdrawalLimit{}, err
	}
	q := s.q
	if tx != nil {
		q = s.q.WithTx(tx)
	}
	if err := q.UpsertWithdrawalLimit(ctx, sqlcgen.UpsertWithdrawalLimitParams{
		TenantID: tenantID, Symbol: in.Asset, KycLevel: in.KYCLevel,
		AutoApproveLimit:    pg.NumericFromAmount(in.AutoApproveLimit),
		DailyLimit:          pg.NumericFromAmount(in.DailyLimit),
		RequireManualReview: in.RequireManualReview,
	}); err != nil {
		return WithdrawalLimit{}, fmt.Errorf("registry: upsert withdrawal limit %s/%d: %w", in.Asset, in.KYCLevel, err)
	}
	r, err := q.GetWithdrawalLimit(ctx, sqlcgen.GetWithdrawalLimitParams{TenantID: tenantID, Symbol: in.Asset, KycLevel: in.KYCLevel})
	if err != nil {
		return WithdrawalLimit{}, mapErr(fmt.Sprintf("withdrawal limit %s/%d", in.Asset, in.KYCLevel), err)
	}
	return withdrawalLimitFromRow(sqlcgen.ListWithdrawalLimitsRow(r))
}

// ListFeeSchedules returns every fee schedule of the tenant, by name.
func (s *Store) ListFeeSchedules(ctx context.Context, tenantID string) ([]FeeSchedule, error) {
	rows, err := s.q.ListFeeSchedules(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("registry: list fee schedules: %w", err)
	}
	out := make([]FeeSchedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, feeScheduleFromRow(r))
	}
	return out, nil
}
