package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

func TestBasic(t *testing.T) {
	p := Basic{}
	active := ledger.Account{Status: ledger.StatusActive}
	frozen := ledger.Account{Status: ledger.StatusFrozen}
	for status, want := range map[string]Reason{
		registry.MarketActive: "", registry.MarketHalted: ReasonMarketNotActive,
		registry.MarketCancelOnly: ReasonMarketNotActive, registry.MarketDelisted: ReasonMarketNotActive,
	} {
		assert.Equal(t, want, p.NewOrder(registry.Market{Status: status}, active), status)
	}
	assert.Equal(t, ReasonAccountFrozen, p.NewOrder(registry.Market{Status: registry.MarketActive}, frozen))
	// a frozen account may still cancel; only delisted markets refuse cancels
	for status, want := range map[string]Reason{
		registry.MarketActive: "", registry.MarketHalted: "", registry.MarketCancelOnly: "", registry.MarketDelisted: ReasonMarketNotActive,
	} {
		assert.Equal(t, want, p.Cancel(registry.Market{Status: status}), status)
	}
}
