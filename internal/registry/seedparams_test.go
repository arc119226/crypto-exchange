package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/money"
)

func writeParams(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "params.json")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// An empty path is the anvil case: every built-in value survives untouched.
func TestSeedParamsAbsentKeepsEveryDefault(t *testing.T) {
	p, err := LoadSeedParams("")
	require.NoError(t, err)

	in, err := p.applyAsset(AssetInput{Symbol: "ETH", MinWithdrawal: seedMinWithdrawalETH, SweepThreshold: seedSweepThresholdETH})
	require.NoError(t, err)
	assert.Equal(t, seedMinWithdrawalETH.String(), in.MinWithdrawal.String())
	assert.Equal(t, seedSweepThresholdETH.String(), in.SweepThreshold.String())

	limits, err := p.applyLimits(seedWithdrawalLimits)
	require.NoError(t, err)
	assert.Equal(t, seedWithdrawalLimits, limits)
}

// The whole point of the format: name one field, change one field.
func TestSeedParamsOverlaysOnlyWhatItNames(t *testing.T) {
	p, err := LoadSeedParams(writeParams(t, `{"assets":{"ETH":{"sweepThreshold":"0.01"}}}`))
	require.NoError(t, err)

	in, err := p.applyAsset(AssetInput{
		Symbol: "ETH", MinWithdrawal: seedMinWithdrawalETH, SweepThreshold: seedSweepThresholdETH,
	})
	require.NoError(t, err)
	assert.Equal(t, "0.01", in.SweepThreshold.String())
	assert.Equal(t, seedMinWithdrawalETH.String(), in.MinWithdrawal.String(), "an unnamed field keeps its default")

	// An asset the file says nothing about is untouched.
	usdc, err := p.applyAsset(AssetInput{Symbol: "USDC", SweepThreshold: seedSweepThresholdUSDC})
	require.NoError(t, err)
	assert.Equal(t, seedSweepThresholdUSDC.String(), usdc.SweepThreshold.String())
}

// Limits merge by (asset, level): listed pairs replace, unlisted ones survive,
// new ones are appended. Replacing the whole table would let a file that only
// meant to adjust ETH drop every USDC row, and a withdrawal with no matching
// limit goes to manual review -- a failure that looks like a queue, not an error.
func TestSeedParamsMergesWithdrawalLimitsByAssetAndLevel(t *testing.T) {
	p, err := LoadSeedParams(writeParams(t, `{"withdrawalLimits":[
		{"asset":"ETH","kycLevel":0,"autoApproveLimit":"0.005","dailyLimit":"0.02"},
		{"asset":"ETH","kycLevel":3,"autoApproveLimit":"1","dailyLimit":"2"}
	]}`))
	require.NoError(t, err)

	limits, err := p.applyLimits(seedWithdrawalLimits)
	require.NoError(t, err)
	require.Len(t, limits, len(seedWithdrawalLimits)+1, "one row replaced, one appended")

	byKey := map[string]WithdrawalLimitInput{}
	for _, l := range limits {
		byKey[l.Asset+":"+string(rune('0'+l.KYCLevel))] = l
	}
	assert.Equal(t, "0.005", byKey["ETH:0"].AutoApproveLimit.String(), "replaced")
	assert.Equal(t, "1", byKey["ETH:1"].AutoApproveLimit.String(), "untouched")
	assert.Equal(t, "200", byKey["USDC:0"].AutoApproveLimit.String(), "another asset is not collateral damage")
	assert.Equal(t, "1", byKey["ETH:3"].AutoApproveLimit.String(), "appended")

	// The built-in order is preserved so two seeds of the same file are
	// identical and a diff shows only what changed.
	assert.Equal(t, seedWithdrawalLimits[1].Asset, limits[1].Asset)
	assert.Equal(t, seedWithdrawalLimits[1].KYCLevel, limits[1].KYCLevel)
}

func TestSeedParamsRejectsWhatItCannotMean(t *testing.T) {
	for name, body := range map[string]string{
		"a misspelled key":      `{"asets":{"ETH":{}}}`,
		"a misspelled field":    `{"assets":{"ETH":{"sweepThreshhold":"1"}}}`,
		"not a number":          `{"assets":{"ETH":{"sweepThreshold":"a lot"}}}`,
		"a negative threshold":  `{"assets":{"ETH":{"sweepThreshold":"-1"}}}`,
		"a zero price tick":     `{"market":{"priceTick":"0"}}`,
		"an unusable asset":     `{"withdrawalLimits":[{"asset":"eth","kycLevel":0,"autoApproveLimit":"1","dailyLimit":"2"}]}`,
		"a cap below the auto":  `{"withdrawalLimits":[{"asset":"ETH","kycLevel":0,"autoApproveLimit":"2","dailyLimit":"1"}]}`,
		"a limit with no daily": `{"withdrawalLimits":[{"asset":"ETH","kycLevel":0,"autoApproveLimit":"1","dailyLimit":""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSeedParams(writeParams(t, body))
			require.Error(t, err)
		})
	}

	_, err := LoadSeedParams(filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err)
}

// The two shipped overlays are part of the deployment surface: a file that no
// longer parses would not be discovered until someone was standing in front of
// a real chain with a faucet cooldown.
func TestShippedSeedParamsAreValid(t *testing.T) {
	anvil, err := LoadSeedParams("../../deploy/seed-params/anvil.json")
	require.NoError(t, err)

	// anvil.json documents the built-in values, so it must apply as a no-op.
	// If a constant in seed.go changes and the file does not, this fails --
	// which is the only thing keeping the documentation honest.
	limits, err := anvil.applyLimits(seedWithdrawalLimits)
	require.NoError(t, err)
	assert.Equal(t, seedWithdrawalLimits, limits)
	for _, in := range []AssetInput{
		{Symbol: "ETH", MinWithdrawal: seedMinWithdrawalETH, SweepThreshold: seedSweepThresholdETH},
		{Symbol: "USDC", MinWithdrawal: seedMinWithdrawalUSDC, SweepThreshold: seedSweepThresholdUSDC},
	} {
		out, err := anvil.applyAsset(in)
		require.NoError(t, err)
		assert.Equal(t, in, out, in.Symbol)
	}
	market, err := anvil.applyMarket(MarketInput{
		PriceTick: seedPriceTick, QtyStep: seedQtyStep, MinNotional: seedMinNotional,
	})
	require.NoError(t, err)
	assert.Equal(t, seedPriceTick.String(), market.PriceTick.String())
	assert.Equal(t, seedQtyStep.String(), market.QtyStep.String())
	assert.Equal(t, seedMinNotional.String(), market.MinNotional.String())

	sepolia, err := LoadSeedParams("../../deploy/seed-params/sepolia.json")
	require.NoError(t, err)
	eth, err := sepolia.applyAsset(AssetInput{Symbol: "ETH", SweepThreshold: seedSweepThresholdETH})
	require.NoError(t, err)
	// A faucet claim is around 0.05 ETH. The threshold has to sit well below
	// one claim or a deposit funded by a faucet is never collected at all.
	assert.Less(t, eth.SweepThreshold.Cmp(money.MustParse("0.05")), 0)
	assert.True(t, eth.SweepThreshold.IsPositive(), "a zero threshold sweeps dust forever")
}
