package registry

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/money"
)

func TestFixtures(t *testing.T) {
	fx, err := LoadFixtures("../../test/fixtures/addresses.dev.json")
	require.NoError(t, err)
	assert.Equal(t, int64(31337), fx.ChainID)
	require.NoError(t, fx.Validate(31337))
	assert.ErrorIs(t, fx.Validate(11155111), ErrInvalid, "chain mismatch must be rejected")

	bad := fx
	bad.USDC = "0x123"
	assert.ErrorIs(t, bad.Validate(31337), ErrInvalid)

	_, err = LoadFixtures(filepath.Join(t.TempDir(), "missing.json"))
	assert.Error(t, err)
	p := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(p, []byte("{"), 0o600))
	_, err = LoadFixtures(p)
	assert.Error(t, err)
}

func TestSeedConstantsRespectPrecisionRule(t *testing.T) {
	// ETH-USDC: scale(qty_step)=4 + scale(price_tick)=2 ≤ USDC scale 6, and qty_step fits ETH scale 18.
	require.NoError(t, ValidatePrecision(seedPriceTick, seedQtyStep, 18, 6))
	assert.ErrorIs(t, ValidatePrecision(money.MustParse("0.001"), seedQtyStep, 18, 6), ErrInvalid)
	assert.ErrorIs(t, ValidatePrecision(seedPriceTick, money.MustParse("0.001"), 2, 6), ErrInvalid)
}

func TestInputValidation(t *testing.T) {
	require.NoError(t, FeeScheduleInput{Name: "default", MakerBps: 10, TakerBps: 20}.Validate())
	assert.ErrorIs(t, FeeScheduleInput{Name: "", MakerBps: 10, TakerBps: 20}.Validate(), ErrInvalid)
	assert.ErrorIs(t, FeeScheduleInput{Name: "x", MakerBps: 10001, TakerBps: 20}.Validate(), ErrInvalid)

	addr := "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	usdc := AssetInput{Symbol: "USDC", Name: "USD Coin", ChainID: 31337, ContractAddress: &addr, Scale: 6, DisplayScale: 2, RequiredConfirmations: 1, Status: AssetActive}
	require.NoError(t, usdc.Validate())
	eth := AssetInput{Symbol: "ETH", Name: "Ether", ChainID: 31337, IsNative: true, Scale: 18, DisplayScale: 6, RequiredConfirmations: 1, Status: AssetActive}
	require.NoError(t, eth.Validate())

	bad := eth
	bad.ContractAddress = &addr
	assert.ErrorIs(t, bad.Validate(), ErrInvalid, "native with contract")
	bad = usdc
	bad.ContractAddress = nil
	assert.ErrorIs(t, bad.Validate(), ErrInvalid, "token without contract")
	bad = usdc
	bad.Symbol = "usdc"
	assert.ErrorIs(t, bad.Validate(), ErrInvalid, "lowercase symbol")
	bad = usdc
	bad.DisplayScale = 7
	assert.ErrorIs(t, bad.Validate(), ErrInvalid, "display scale > scale")
	bad = usdc
	bad.Scale = 19
	assert.ErrorIs(t, bad.Validate(), ErrInvalid, "scale > 18")
	bad = usdc
	bad.RequiredConfirmations = 0
	assert.ErrorIs(t, bad.Validate(), ErrInvalid)
	bad = usdc
	bad.MinDeposit = money.MustParse("-1")
	assert.ErrorIs(t, bad.Validate(), ErrInvalid)
	bad = usdc
	bad.Status = "paused"
	assert.ErrorIs(t, bad.Validate(), ErrInvalid)
	bad = usdc
	bad.Name = ""
	assert.ErrorIs(t, bad.Validate(), ErrInvalid)

	m := MarketInput{Symbol: "ETH-USDC", BaseSymbol: "ETH", QuoteSymbol: "USDC", PriceTick: seedPriceTick, QtyStep: seedQtyStep, MinNotional: seedMinNotional, FeeSchedule: "default", SelfTradePolicy: STPCancelNewest, Status: MarketActive}
	require.NoError(t, m.Validate())
	badM := m
	badM.Symbol = "ETHUSDC"
	assert.ErrorIs(t, badM.Validate(), ErrInvalid, "symbol must be BASE-QUOTE")
	badM = m
	badM.QuoteSymbol = "ETH"
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	badM = m
	badM.PriceTick = money.Zero
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	badM = m
	badM.SelfTradePolicy = "reject"
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	// The engine implements cancel_newest and nothing else: matching rejects
	// cancel_oldest outright, and allow suppresses nothing, so a market saved
	// with either is a market the engine cannot run. Validation is the only
	// place that can say so before the row exists.
	for _, stp := range []string{STPCancelOldest, STPAllow} {
		badM = m
		badM.SelfTradePolicy = stp
		assert.ErrorIs(t, badM.Validate(), ErrInvalid, "self_trade_policy %q is not implemented", stp)
	}
	badM = m
	badM.Status = "open"
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	badM = m
	neg := money.MustParse("-1")
	badM.MaxQty = &neg
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	badM = m
	bps := int32(0)
	badM.MaxSlippageBps = &bps
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
	badM = m
	badM.FeeSchedule = ""
	assert.ErrorIs(t, badM.Validate(), ErrInvalid)
}

func TestCache(t *testing.T) {
	c := NewCache("default")
	_, ok := c.Market("ETH-USDC")
	assert.False(t, ok)
	assert.Empty(t, c.Markets())
	err := c.Load(t.Context(), fakeReader{
		assets:  []Asset{{Symbol: "ETH", Scale: 18}, {Symbol: "USDC", Scale: 6}},
		markets: []Market{{Symbol: "ETH-USDC", BaseSymbol: "ETH", QuoteSymbol: "USDC"}},
	})
	require.NoError(t, err)
	m, ok := c.Market("ETH-USDC")
	assert.True(t, ok)
	assert.Equal(t, "ETH", m.BaseSymbol)
	a, ok := c.Asset("USDC")
	assert.True(t, ok)
	assert.Equal(t, int32(6), a.Scale)
	assert.Equal(t, money.Asset{Symbol: "USDC", Scale: 6}, a.Money())
	assert.Len(t, c.Markets(), 1)
}

type fakeReader struct {
	assets  []Asset
	markets []Market
}

func (f fakeReader) ListAssets(context.Context, string) ([]Asset, error) { return f.assets, nil }
func (f fakeReader) GetAsset(_ context.Context, _, symbol string) (Asset, error) {
	for _, a := range f.assets {
		if a.Symbol == symbol {
			return a, nil
		}
	}
	return Asset{}, ErrNotFound
}
func (f fakeReader) ListMarkets(context.Context, string) ([]Market, error) { return f.markets, nil }
func (f fakeReader) GetMarket(_ context.Context, _, symbol string) (Market, error) {
	for _, m := range f.markets {
		if m.Symbol == symbol {
			return m, nil
		}
	}
	return Market{}, ErrNotFound
}
