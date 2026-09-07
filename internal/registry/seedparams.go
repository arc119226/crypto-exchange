package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// SeedParams overlays the built-in seed values in seed.go.
//
// Those values were chosen for anvil, where a test account can hold ten
// thousand ether: a 0.05 ETH sweep threshold and a 0.1 ETH auto-approve
// ceiling exercise both branches of every policy without anyone thinking
// about supply. On a testnet funded by faucets that hand out 0.05 a day, the
// same numbers mean nothing can be swept and nothing auto-approves, and the
// operator has no way to say so short of editing Go constants.
//
// Every field is optional and absence means "keep the built-in value", so an
// overlay file says only what differs from the default. That is deliberate:
// a file that had to restate every value would drift from the defaults
// silently, and reading it would not tell you what this chain does
// differently.
type SeedParams struct {
	// Assets is keyed by symbol, and an unknown symbol is an error rather
	// than a no-op: a typo that quietly changes nothing is exactly the kind
	// of thing that is discovered by a withdrawal behaving unexpectedly.
	Assets map[string]AssetParams `json:"assets"`
	Market *MarketParams          `json:"market"`
	// WithdrawalLimits merges into the built-in table by (asset, kycLevel):
	// a listed pair replaces the built-in row, an unlisted one keeps it, and
	// a new pair is added. Replacing the whole table instead would let a file
	// that only meant to adjust ETH drop every USDC row -- and a withdrawal
	// with no matching limit goes to manual review, so the damage would show
	// up as a queue rather than as an error.
	WithdrawalLimits []WithdrawalLimitParams `json:"withdrawalLimits"`
}

// AssetParams overrides the per-asset thresholds. Amounts are decimal strings
// so a JSON file can carry the full NUMERIC(36,18) range; float64 could not.
type AssetParams struct {
	MinDeposit     *string `json:"minDeposit"`
	MinWithdrawal  *string `json:"minWithdrawal"`
	WithdrawalFee  *string `json:"withdrawalFee"`
	SweepThreshold *string `json:"sweepThreshold"`
}

// MarketParams overrides the ETH-USDC market's trading increments.
type MarketParams struct {
	PriceTick   *string `json:"priceTick"`
	QtyStep     *string `json:"qtyStep"`
	MinNotional *string `json:"minNotional"`
}

// WithdrawalLimitParams is one row of the per-asset, per-KYC-level table.
// Unlike the two above, both amounts are required: a limit row with half its
// numbers missing has no meaning to fall back on.
type WithdrawalLimitParams struct {
	Asset               string `json:"asset"`
	KYCLevel            int16  `json:"kycLevel"`
	AutoApproveLimit    string `json:"autoApproveLimit"`
	DailyLimit          string `json:"dailyLimit"`
	RequireManualReview bool   `json:"requireManualReview"`
}

// LoadSeedParams reads an overlay file. An empty path is not an error: it
// means the built-in values, which is what every anvil run uses.
func LoadSeedParams(path string) (SeedParams, error) {
	if path == "" {
		return SeedParams{}, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return SeedParams{}, fmt.Errorf("seed params: %w", err)
	}
	var p SeedParams
	dec := json.NewDecoder(bytes.NewReader(b))
	// A misspelled key would otherwise be accepted and ignored, which for a
	// file whose whole purpose is "say only what differs" is the worst
	// possible behaviour: the difference silently does not happen.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return SeedParams{}, fmt.Errorf("seed params: decode %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return SeedParams{}, err
	}
	return p, nil
}

// Validate parses every amount and rejects the combinations that cannot mean
// anything, so a bad file fails at load rather than half-way through the seed
// transaction.
func (p SeedParams) Validate() error {
	for symbol, a := range p.Assets {
		for name, v := range map[string]*string{
			"minDeposit": a.MinDeposit, "minWithdrawal": a.MinWithdrawal,
			"withdrawalFee": a.WithdrawalFee, "sweepThreshold": a.SweepThreshold,
		} {
			if _, err := parseSeedAmount(fmt.Sprintf("assets.%s.%s", symbol, name), v); err != nil {
				return err
			}
		}
	}
	if p.Market != nil {
		for name, v := range map[string]*string{
			"priceTick": p.Market.PriceTick, "qtyStep": p.Market.QtyStep,
			"minNotional": p.Market.MinNotional,
		} {
			amount, err := parseSeedAmount("market."+name, v)
			if err != nil {
				return err
			}
			if v != nil && !amount.IsPositive() {
				return fmt.Errorf("%w: market.%s must be positive", ErrInvalid, name)
			}
		}
	}
	for i, l := range p.WithdrawalLimits {
		if !symbolPattern.MatchString(l.Asset) {
			return fmt.Errorf("%w: withdrawalLimits[%d].asset %q is not an asset symbol", ErrInvalid, i, l.Asset)
		}
		if l.KYCLevel < 0 {
			return fmt.Errorf("%w: withdrawalLimits[%d].kycLevel must not be negative", ErrInvalid, i)
		}
		auto, err := parseSeedAmount(fmt.Sprintf("withdrawalLimits[%d].autoApproveLimit", i), &l.AutoApproveLimit)
		if err != nil {
			return err
		}
		daily, err := parseSeedAmount(fmt.Sprintf("withdrawalLimits[%d].dailyLimit", i), &l.DailyLimit)
		if err != nil {
			return err
		}
		// An auto-approve ceiling above the daily cap can never be reached:
		// the daily check would reject first. Saying so is more useful than
		// seeding a row that does not do what it looks like it does.
		if auto.Cmp(daily) > 0 {
			return fmt.Errorf("%w: withdrawalLimits[%d] auto-approves %s but caps the day at %s",
				ErrInvalid, i, auto, daily)
		}
	}
	return nil
}

// parseSeedAmount decodes one optional decimal string. A nil pointer is the
// zero amount and no error, which callers read as "not overridden".
func parseSeedAmount(field string, v *string) (money.Amount, error) {
	if v == nil {
		return money.Zero, nil
	}
	a, err := money.ParseAmount(*v)
	if err != nil {
		return money.Zero, fmt.Errorf("%w: %s %q is not a decimal amount", ErrInvalid, field, *v)
	}
	if a.IsNegative() {
		return money.Zero, fmt.Errorf("%w: %s %q must not be negative", ErrInvalid, field, *v)
	}
	return a, nil
}

// applyAsset overlays this file's values for one asset onto the built-in input.
func (p SeedParams) applyAsset(in AssetInput) (AssetInput, error) {
	a, ok := p.Assets[in.Symbol]
	if !ok {
		return in, nil
	}
	for _, f := range []struct {
		name string
		src  *string
		dst  *money.Amount
	}{
		{"minDeposit", a.MinDeposit, &in.MinDeposit},
		{"minWithdrawal", a.MinWithdrawal, &in.MinWithdrawal},
		{"withdrawalFee", a.WithdrawalFee, &in.WithdrawalFee},
		{"sweepThreshold", a.SweepThreshold, &in.SweepThreshold},
	} {
		if f.src == nil {
			continue
		}
		v, err := parseSeedAmount(fmt.Sprintf("assets.%s.%s", in.Symbol, f.name), f.src)
		if err != nil {
			return in, err
		}
		*f.dst = v
	}
	return in, nil
}

// applyMarket overlays the market increments.
func (p SeedParams) applyMarket(in MarketInput) (MarketInput, error) {
	if p.Market == nil {
		return in, nil
	}
	for _, f := range []struct {
		name string
		src  *string
		dst  *money.Amount
	}{
		{"priceTick", p.Market.PriceTick, &in.PriceTick},
		{"qtyStep", p.Market.QtyStep, &in.QtyStep},
		{"minNotional", p.Market.MinNotional, &in.MinNotional},
	} {
		if f.src == nil {
			continue
		}
		v, err := parseSeedAmount("market."+f.name, f.src)
		if err != nil {
			return in, err
		}
		*f.dst = v
	}
	return in, nil
}

// applyLimits merges the overlay rows into the built-in table, keeping the
// built-in order so that seeding stays deterministic and a diff of two runs
// shows only what changed.
func (p SeedParams) applyLimits(base []WithdrawalLimitInput) ([]WithdrawalLimitInput, error) {
	type key struct {
		asset string
		level int16
	}
	overlay := make(map[key]WithdrawalLimitInput, len(p.WithdrawalLimits))
	order := make([]key, 0, len(p.WithdrawalLimits))
	for i, l := range p.WithdrawalLimits {
		auto, err := parseSeedAmount(fmt.Sprintf("withdrawalLimits[%d].autoApproveLimit", i), &l.AutoApproveLimit)
		if err != nil {
			return nil, err
		}
		daily, err := parseSeedAmount(fmt.Sprintf("withdrawalLimits[%d].dailyLimit", i), &l.DailyLimit)
		if err != nil {
			return nil, err
		}
		k := key{l.Asset, l.KYCLevel}
		overlay[k] = WithdrawalLimitInput{
			Asset: l.Asset, KYCLevel: l.KYCLevel, AutoApproveLimit: auto,
			DailyLimit: daily, RequireManualReview: l.RequireManualReview,
		}
		order = append(order, k)
	}
	out := make([]WithdrawalLimitInput, 0, len(base)+len(overlay))
	for _, in := range base {
		k := key{in.Asset, in.KYCLevel}
		if over, ok := overlay[k]; ok {
			out = append(out, over)
			delete(overlay, k)
			continue
		}
		out = append(out, in)
	}
	// Whatever is left names a pair the built-in table does not have.
	for _, k := range order {
		if over, ok := overlay[k]; ok {
			out = append(out, over)
			delete(overlay, k)
		}
	}
	return out, nil
}
