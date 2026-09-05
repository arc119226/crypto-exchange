package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Fixtures is the addresses.json written by infra/contracts/script/Deploy.s.sol
// (vm.writeJson) and consumed by `exchange seed`.
type Fixtures struct {
	ChainID         int64  `json:"chainId"`
	Deployer        string `json:"deployer"`
	HotWallet       string `json:"hotWallet"`
	USDC            string `json:"usdc"`
	DeployedAtBlock uint64 `json:"deployedAtBlock"`
}

// LoadFixtures reads and decodes the fixtures file.
func LoadFixtures(path string) (Fixtures, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return Fixtures{}, fmt.Errorf("fixtures: %w", err)
	}
	var fx Fixtures
	if err := json.Unmarshal(b, &fx); err != nil {
		return Fixtures{}, fmt.Errorf("fixtures: decode %s: %w", path, err)
	}
	return fx, nil
}

// Validate checks addresses and that the file belongs to the expected chain,
// so a stale addresses.json is never applied to a rebuilt anvil.
func (f Fixtures) Validate(expectedChainID int64) error {
	if f.ChainID != expectedChainID {
		return fmt.Errorf("%w: fixtures are for chain %d but ETH_CHAIN_ID is %d (run `make reset`?)", ErrInvalid, f.ChainID, expectedChainID)
	}
	for name, addr := range map[string]string{"deployer": f.Deployer, "hotWallet": f.HotWallet, "usdc": f.USDC} {
		if !addressPattern.MatchString(addr) {
			return fmt.Errorf("%w: fixtures.%s %q is not a 0x address", ErrInvalid, name, addr)
		}
	}
	return nil
}

// Seed constants (docs/plan-v1.0.md §3.1, §6.6). Changing a value here
// changes what `exchange seed` upserts; the seed itself is idempotent.
const (
	DefaultFeeScheduleName = "default"
	DefaultMakerBps        = 10
	DefaultTakerBps        = 20
	DefaultMarketSymbol    = "ETH-USDC"
)

var (
	seedPriceTick   = money.MustParse("0.01")
	seedQtyStep     = money.MustParse("0.0001")
	seedMinNotional = money.MustParse("5")
)

// SeedOptions parameterise Seed.
type SeedOptions struct {
	TenantID              string
	RequiredConfirmations int32 // anvil: 1, Sepolia: 6+
}

// SeedResult summarises what was upserted.
type SeedResult struct {
	FeeSchedules int
	Assets       int
	Markets      int
}

// Seed upserts the default fee schedule, ETH, USDC and the ETH-USDC market
// in one transaction. Running it twice is a no-op (versions do not change).
func Seed(ctx context.Context, pool *pgxpool.Pool, fx Fixtures, opts SeedOptions) (SeedResult, error) {
	if opts.TenantID == "" {
		opts.TenantID = "default"
	}
	if opts.RequiredConfirmations <= 0 {
		opts.RequiredConfirmations = 1
	}
	store := NewStore(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return SeedResult{}, fmt.Errorf("seed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var res SeedResult
	if _, err := store.UpsertFeeSchedule(ctx, tx, opts.TenantID, FeeScheduleInput{Name: DefaultFeeScheduleName, MakerBps: DefaultMakerBps, TakerBps: DefaultTakerBps}); err != nil {
		return res, err
	}
	res.FeeSchedules++

	usdc := fx.USDC
	for _, in := range []AssetInput{
		{Symbol: "ETH", Name: "Ether", ChainID: fx.ChainID, IsNative: true, Scale: 18, DisplayScale: 6,
			RequiredConfirmations: opts.RequiredConfirmations, DepositEnabled: true, WithdrawEnabled: true, Status: AssetActive},
		{Symbol: "USDC", Name: "Mock USD Coin", ChainID: fx.ChainID, ContractAddress: &usdc, Scale: 6, DisplayScale: 2,
			RequiredConfirmations: opts.RequiredConfirmations, DepositEnabled: true, WithdrawEnabled: true, Status: AssetActive},
	} {
		if _, err := store.UpsertAsset(ctx, tx, opts.TenantID, in); err != nil {
			return res, err
		}
		res.Assets++
	}

	if _, err := store.UpsertMarket(ctx, tx, opts.TenantID, MarketInput{
		Symbol: DefaultMarketSymbol, BaseSymbol: "ETH", QuoteSymbol: "USDC",
		PriceTick: seedPriceTick, QtyStep: seedQtyStep, MinNotional: seedMinNotional,
		FeeSchedule: DefaultFeeScheduleName, SelfTradePolicy: STPCancelNewest, Status: MarketActive,
	}); err != nil {
		return res, err
	}
	res.Markets++

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("seed: commit: %w", err)
	}
	return res, nil
}
