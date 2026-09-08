//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The registry, editable: every write leaves an audit row and, where the
// engine has to know, one event; a write that changes nothing leaves
// neither. Seeded with the dev fixtures first (ETH, USDC, ETH-USDC, the
// default schedule, six limits).
func TestAdminRegistryWrites(t *testing.T) {
	ctx := context.Background()
	h := setupLedger(t)
	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, h.all, fx, registry.SeedOptions{TenantID: "default", RequiredConfirmations: 1})
	require.NoError(t, err)
	srv := adminServer(t, h)
	put := func(path string, body any) adminResp { return call(t, srv, http.MethodPut, path, adminKey, body) }
	post := func(path string, body any) adminResp { return call(t, srv, http.MethodPost, path, adminKey, body) }
	get := func(path string) adminResp { return call(t, srv, http.MethodGet, path, adminKey, nil) }

	t.Run("what the seed left", func(t *testing.T) {
		assert.Len(t, decode[gen.AssetList](t, get("/admin/v1/assets")).Assets, 2)
		assert.Len(t, decode[gen.FeeScheduleList](t, get("/admin/v1/fee-schedules")).FeeSchedules, 1)
		assert.Len(t, decode[gen.WithdrawalLimitList](t, get("/admin/v1/withdrawal-limits")).WithdrawalLimits, 6)
		m := decode[gen.Market](t, get("/admin/v1/markets/ETH-USDC"))
		require.NotNil(t, m.FeeSchedule)
		assert.Equal(t, "default", *m.FeeSchedule)
		assert.Equal(t, http.StatusNotFound, get("/admin/v1/markets/BTC-USDC").status)
		assert.Equal(t, http.StatusNotFound, get("/admin/v1/assets/BTC").status)
	})

	addr := "0x912CE59144191C1204E64559FE8253a0e49E6548"
	arb := map[string]any{
		"symbol": "ARB", "name": "Arbitrum", "chain_id": 31337, "contract_address": addr, "is_native": false,
		"scale": 18, "display_scale": 4, "required_confirmations": 12,
		"min_deposit": "0", "min_withdrawal": "1", "withdrawal_fee": "0.1", "sweep_threshold": "50",
		"deposit_enabled": true, "withdraw_enabled": true, "status": "active", "reason": "listing ARB",
	}
	t.Run("an asset is created once", func(t *testing.T) {
		r := post("/admin/v1/assets", arb)
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		a := decode[gen.Asset](t, r)
		assert.Equal(t, "ARB", a.Symbol)
		assert.Equal(t, int32(1), a.Version)
		assert.Equal(t, http.StatusConflict, post("/admin/v1/assets", arb).status, "POST twice is a conflict, not a second row")
		assert.Equal(t, []string{"asset.create"}, auditActions(t, h.all, "asset."))
		assert.Equal(t, []string{"asset.updated"}, outboxTypes(t, h.all, "asset."))

		bad := map[string]any{}
		for k, v := range arb {
			bad[k] = v
		}
		bad["symbol"], bad["scale"] = "BAD", 40
		assert.Equal(t, http.StatusBadRequest, post("/admin/v1/assets", bad).status)
	})

	t.Run("editing an asset names what changed, and nothing twice", func(t *testing.T) {
		before := decode[gen.Asset](t, get("/admin/v1/assets/USDC"))
		body := map[string]any{
			"name": before.Name, "chain_id": before.ChainID, "contract_address": before.ContractAddress, "is_native": false,
			"scale": before.Scale, "display_scale": before.DisplayScale, "required_confirmations": before.RequiredConfirmations,
			"min_deposit": before.MinDeposit, "min_withdrawal": before.MinWithdrawal, "withdrawal_fee": before.WithdrawalFee,
			"sweep_threshold": before.SweepThreshold, "deposit_enabled": true, "withdraw_enabled": false, "status": "active",
			"reason": "issuer maintenance window",
		}
		r := put("/admin/v1/assets/USDC", body)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		after := decode[gen.Asset](t, r)
		assert.False(t, after.WithdrawEnabled)
		assert.Equal(t, before.Version+1, after.Version)

		r = put("/admin/v1/assets/USDC", body)
		require.Equal(t, http.StatusOK, r.status)
		assert.Equal(t, after.Version, decode[gen.Asset](t, r).Version, "the same body again is not a change")
		assert.Equal(t, []string{"asset.create", "asset.update"}, auditActions(t, h.all, "asset."))
		assert.Equal(t, []string{"asset.updated", "asset.updated"}, outboxTypes(t, h.all, "asset."))

		var changed []string
		require.NoError(t, h.all.QueryRow(ctx,
			`SELECT payload->'changed_fields' FROM eventbus.outbox WHERE event_type = 'asset.updated' ORDER BY id DESC LIMIT 1`).Scan(&changed))
		assert.Equal(t, []string{"withdraw_enabled"}, changed)

		assert.Equal(t, http.StatusNotFound, put("/admin/v1/assets/BTC", body).status)
		body["reason"] = ""
		assert.Equal(t, http.StatusBadRequest, put("/admin/v1/assets/USDC", body).status, "no reason, no write")
	})

	t.Run("a market is created against existing assets and a schedule", func(t *testing.T) {
		body := map[string]any{
			"symbol": "ARB-USDC", "base_asset": "ARB", "quote_asset": "USDC", "price_tick": "0.0001", "qty_step": "0.01",
			"min_notional": "5", "fee_schedule": "default", "self_trade_policy": "cancel_newest", "status": "halted",
			"reason": "listing, opens next week",
		}
		r := post("/admin/v1/markets", body)
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		m := decode[gen.Market](t, r)
		assert.Equal(t, gen.MarketStatusHalted, m.Status)
		assert.Equal(t, http.StatusConflict, post("/admin/v1/markets", body).status)

		missing := map[string]any{}
		for k, v := range body {
			missing[k] = v
		}
		missing["symbol"], missing["base_asset"] = "BTC-USDC", "BTC"
		r = post("/admin/v1/markets", missing)
		assert.Equal(t, http.StatusNotFound, r.status, "a market cannot reference an asset that does not exist")
		assert.Contains(t, string(r.body), "base asset BTC")
		missing["symbol"], missing["base_asset"], missing["fee_schedule"] = "ARB-ETH", "ARB", "vip"
		missing["quote_asset"] = "ETH"
		assert.Equal(t, http.StatusNotFound, post("/admin/v1/markets", missing).status, "nor a fee schedule")

		body["max_slippage_bps"], body["status"], body["reason"] = 300, "active", "open"
		r = put("/admin/v1/markets/ARB-USDC", body)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		m = decode[gen.Market](t, r)
		require.NotNil(t, m.MaxSlippageBps)
		assert.Equal(t, int32(300), *m.MaxSlippageBps)
		assert.Equal(t, gen.MarketStatusActive, m.Status)
		var changed []string
		require.NoError(t, h.all.QueryRow(ctx,
			`SELECT payload->'changed_fields' FROM eventbus.outbox WHERE event_type = 'market.updated' ORDER BY id DESC LIMIT 1`).Scan(&changed))
		assert.Equal(t, []string{"max_slippage_bps", "status"}, changed)
		assert.Equal(t, []string{"market.create", "market.update"}, auditActions(t, h.all, "market."))

		body["price_tick"] = "0.00000001"
		assert.Equal(t, http.StatusBadRequest, put("/admin/v1/markets/ARB-USDC", body).status, "precision must fit the quote asset")
	})

	t.Run("a fee schedule change reaches every market on it", func(t *testing.T) {
		r := post("/admin/v1/fee-schedules", map[string]any{"name": "vip", "maker_bps": 2, "taker_bps": 8, "reason": "market makers"})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		assert.Equal(t, http.StatusConflict, post("/admin/v1/fee-schedules", map[string]any{"name": "vip", "maker_bps": 2, "taker_bps": 8, "reason": "again"}).status)

		r = put("/admin/v1/fee-schedules/default", map[string]any{"maker_bps": 8, "taker_bps": 18, "reason": "Q4 pricing"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		f := decode[gen.FeeSchedule](t, r)
		assert.Equal(t, int32(8), f.MakerBps)
		assert.Equal(t, int32(2), f.Version)
		for _, symbol := range []string{"ETH-USDC", "ARB-USDC"} {
			m := decode[gen.Market](t, get("/admin/v1/markets/"+symbol))
			assert.Equal(t, int32(8), m.MakerBps, symbol)
			assert.Equal(t, int32(18), m.TakerBps, symbol)
		}
		assert.Equal(t, []string{"fee_schedule.updated", "fee_schedule.updated"}, outboxTypes(t, h.all, "fee_schedule."), "one for the create, one for the change; none per market")
		assert.Equal(t, http.StatusNotFound, put("/admin/v1/fee-schedules/nope", map[string]any{"maker_bps": 1, "taker_bps": 1, "reason": "x"}).status)
		assert.Equal(t, http.StatusBadRequest, put("/admin/v1/fee-schedules/default", map[string]any{"maker_bps": 20000, "taker_bps": 1, "reason": "x"}).status)
	})

	t.Run("withdrawal limits", func(t *testing.T) {
		r := put("/admin/v1/withdrawal-limits/ETH/1", map[string]any{"auto_approve_limit": "2", "daily_limit": "20", "require_manual_review": false, "reason": "raise level 1"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		l := decode[gen.WithdrawalLimit](t, r)
		assert.Equal(t, "2", l.AutoApproveLimit.String())
		assert.Equal(t, int32(2), l.Version)

		r = put("/admin/v1/withdrawal-limits/ARB/0", map[string]any{"auto_approve_limit": "100", "daily_limit": "1000", "require_manual_review": true, "reason": "new asset"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Equal(t, int32(1), decode[gen.WithdrawalLimit](t, r).Version, "created on first PUT")
		assert.Len(t, decode[gen.WithdrawalLimitList](t, get("/admin/v1/withdrawal-limits")).WithdrawalLimits, 7)

		assert.Equal(t, http.StatusNotFound, put("/admin/v1/withdrawal-limits/BTC/0", map[string]any{"auto_approve_limit": "1", "daily_limit": "2", "require_manual_review": false, "reason": "x"}).status)
		assert.Equal(t, http.StatusBadRequest, put("/admin/v1/withdrawal-limits/ETH/0", map[string]any{"auto_approve_limit": "5", "daily_limit": "2", "require_manual_review": false, "reason": "x"}).status, "auto-approve above daily can never bind")
		assert.Equal(t, http.StatusBadRequest, put("/admin/v1/withdrawal-limits/ETH/3", map[string]any{"auto_approve_limit": "1", "daily_limit": "2", "require_manual_review": false, "reason": "x"}).status)
		assert.Equal(t, []string{"withdrawal_limit.update", "withdrawal_limit.create"}, auditActions(t, h.all, "withdrawal_limit."))
		assert.Empty(t, outboxTypes(t, h.all, "withdrawal_limit"), "nothing caches limits, so no event")
	})

	t.Run("a reload request is an event and an audit row", func(t *testing.T) {
		r := post("/admin/v1/engine/reload", map[string]any{"reason": "seeded by hand"})
		require.Equal(t, http.StatusAccepted, r.status, string(r.body))
		acc := decode[gen.ReloadAccepted](t, r)
		assert.NotEmpty(t, acc.EventID)
		assert.Equal(t, []string{"registry.reload"}, outboxTypes(t, h.all, "registry."))
		assert.Equal(t, []string{"engine.reload"}, auditActions(t, h.all, "engine."))
		assert.Equal(t, http.StatusBadRequest, post("/admin/v1/engine/reload", map[string]any{}).status)
	})
}
