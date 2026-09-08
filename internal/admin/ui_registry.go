package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The registry pages: assets, markets, fee schedules, withdrawal limits.
// Each is a table with the edit form under every row and a "new" form at the
// bottom; every submit is a POST that redirects back with a flash, through
// the same writes the REST endpoints use.

var (
	assetStatuses  = []string{registry.AssetActive, registry.AssetDisabled}
	marketStatuses = []string{registry.MarketActive, registry.MarketHalted, registry.MarketCancelOnly, registry.MarketDelisted}
	stpPolicies    = []string{registry.STPCancelNewest, registry.STPCancelOldest, registry.STPAllow}
	kycLevels      = []int{0, 1, 2}
)

type assetsData struct {
	Assets   []registry.Asset
	Statuses []string
}

type marketsData struct {
	Markets      []registry.Market
	Assets       []registry.Asset
	FeeSchedules []registry.FeeSchedule
	Statuses     []string
	Policies     []string
}

type feeSchedulesData struct {
	FeeSchedules []registry.FeeSchedule
}

type withdrawalLimitsData struct {
	Limits []registry.WithdrawalLimit
	Assets []registry.Asset
	Levels []int
}

func (u *UI) assets(w http.ResponseWriter, r *http.Request) {
	assets, err := u.h.registry.ListAssets(r.Context(), u.h.tenant)
	if err != nil {
		u.fail(w, r, "assets", "list assets", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "assets", view{Title: "Assets", Data: assetsData{Assets: assets, Statuses: assetStatuses}})
}

func (u *UI) createAsset(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := assetForm(f, f.str("symbol"))
	if f.problem != "" {
		u.bounce(w, r, "/admin/assets", f.problem)
		return
	}
	_, err := u.h.upsertAsset(r.Context(), in, true, f.reason())
	u.done(w, r, "/admin/assets", err, "Asset "+in.Symbol+" listed.")
}

func (u *UI) updateAsset(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := assetForm(f, chi.URLParam(r, "symbol"))
	if f.problem != "" {
		u.bounce(w, r, "/admin/assets", f.problem)
		return
	}
	_, err := u.h.upsertAsset(r.Context(), in, false, f.reason())
	u.done(w, r, "/admin/assets", err, "Asset "+in.Symbol+" saved.")
}

func assetForm(f *form, symbol string) registry.AssetInput {
	return registry.AssetInput{
		Symbol: strings.ToUpper(strings.TrimSpace(symbol)), Name: f.str("name"), ChainID: f.i64("chain_id"),
		ContractAddress: f.optStr("contract_address"), IsNative: f.boolean("is_native"),
		Scale: f.i32("scale"), DisplayScale: f.i32("display_scale"), RequiredConfirmations: f.i32("required_confirmations"),
		MinDeposit: f.amount("min_deposit"), MinWithdrawal: f.amount("min_withdrawal"),
		WithdrawalFee: f.amount("withdrawal_fee"), SweepThreshold: f.amount("sweep_threshold"),
		DepositEnabled: f.boolean("deposit_enabled"), WithdrawEnabled: f.boolean("withdraw_enabled"), Status: f.str("status"),
	}
}

func (u *UI) markets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	markets, err := u.h.registry.ListMarkets(ctx, u.h.tenant)
	if err != nil {
		u.fail(w, r, "markets", "list markets", err)
		return
	}
	assets, err := u.h.registry.ListAssets(ctx, u.h.tenant)
	if err != nil {
		u.fail(w, r, "markets", "list assets", err)
		return
	}
	fees, err := u.h.registry.ListFeeSchedules(ctx, u.h.tenant)
	if err != nil {
		u.fail(w, r, "markets", "list fee schedules", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "markets", view{Title: "Markets", Data: marketsData{
		Markets: markets, Assets: assets, FeeSchedules: fees, Statuses: marketStatuses, Policies: stpPolicies,
	}})
}

func (u *UI) createMarket(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := marketForm(f, f.str("base_asset")+"-"+f.str("quote_asset"))
	if f.problem != "" {
		u.bounce(w, r, "/admin/markets", f.problem)
		return
	}
	_, err := u.h.upsertMarket(r.Context(), in, true, f.reason())
	u.done(w, r, "/admin/markets", err, "Market "+in.Symbol+" listed; the engine opens the book on reload.")
}

func (u *UI) updateMarket(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := marketForm(f, chi.URLParam(r, "symbol"))
	if f.problem != "" {
		u.bounce(w, r, "/admin/markets", f.problem)
		return
	}
	_, err := u.h.upsertMarket(r.Context(), in, false, f.reason())
	u.done(w, r, "/admin/markets", err, "Market "+in.Symbol+" saved.")
}

func marketForm(f *form, symbol string) registry.MarketInput {
	return registry.MarketInput{
		Symbol: strings.ToUpper(strings.TrimSpace(symbol)), BaseSymbol: f.str("base_asset"), QuoteSymbol: f.str("quote_asset"),
		PriceTick: f.amount("price_tick"), QtyStep: f.amount("qty_step"), MinNotional: f.amount("min_notional"),
		MaxQty: f.optAmount("max_qty"), MaxSlippageBps: f.optI32("max_slippage_bps"),
		FeeSchedule: f.str("fee_schedule"), SelfTradePolicy: f.str("self_trade_policy"), Status: f.str("status"),
	}
}

func (u *UI) setMarketStatusPage(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	symbol := chi.URLParam(r, "symbol")
	status := f.str("status")
	if f.problem != "" {
		u.bounce(w, r, "/admin/markets", f.problem)
		return
	}
	if !registry.ValidMarketStatus(status) {
		u.bounce(w, r, "/admin/markets", "Unknown market status "+status+".")
		return
	}
	_, err := u.h.setMarketStatus(r.Context(), symbol, status, f.reason())
	u.done(w, r, "/admin/markets", err, "Market "+symbol+" is now "+status+".")
}

func (u *UI) reload(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	reason := f.reason()
	if f.problem != "" {
		u.bounce(w, r, "/admin/markets", f.problem)
		return
	}
	evt, err := u.h.requestReload(r.Context(), reason)
	u.done(w, r, "/admin/markets", err, "Reload requested; event "+evt.EventID+" is in the outbox.")
}

func (u *UI) feeSchedules(w http.ResponseWriter, r *http.Request) {
	fees, err := u.h.registry.ListFeeSchedules(r.Context(), u.h.tenant)
	if err != nil {
		u.fail(w, r, "fee-schedules", "list fee schedules", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "fee-schedules", view{Title: "Fee schedules", Data: feeSchedulesData{FeeSchedules: fees}})
}

func (u *UI) createFeeSchedule(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := registry.FeeScheduleInput{Name: f.str("name"), MakerBps: f.i32("maker_bps"), TakerBps: f.i32("taker_bps")}
	if f.problem != "" {
		u.bounce(w, r, "/admin/fee-schedules", f.problem)
		return
	}
	_, err := u.h.upsertFeeSchedule(r.Context(), in, true, f.reason())
	u.done(w, r, "/admin/fee-schedules", err, "Fee schedule "+in.Name+" added.")
}

func (u *UI) updateFeeSchedule(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	in := registry.FeeScheduleInput{Name: chi.URLParam(r, "name"), MakerBps: f.i32("maker_bps"), TakerBps: f.i32("taker_bps")}
	if f.problem != "" {
		u.bounce(w, r, "/admin/fee-schedules", f.problem)
		return
	}
	_, err := u.h.upsertFeeSchedule(r.Context(), in, false, f.reason())
	u.done(w, r, "/admin/fee-schedules", err, "Fee schedule "+in.Name+" saved; markets on it pay the new rates from the next trade.")
}

func (u *UI) withdrawalLimits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limits, err := u.h.registry.ListWithdrawalLimits(ctx, u.h.tenant)
	if err != nil {
		u.fail(w, r, "withdrawal-limits", "list withdrawal limits", err)
		return
	}
	assets, err := u.h.registry.ListAssets(ctx, u.h.tenant)
	if err != nil {
		u.fail(w, r, "withdrawal-limits", "list assets", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "withdrawal-limits", view{Title: "Withdrawal limits", Data: withdrawalLimitsData{
		Limits: limits, Assets: assets, Levels: kycLevels,
	}})
}

func (u *UI) setWithdrawalLimitPage(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	asset, level := chi.URLParam(r, "asset"), f.i32("kyc_level")
	if asset == "" {
		asset = f.str("asset")
	}
	if level < 0 || level > 2 {
		f.fail("kyc_level must be 0, 1 or 2")
	}
	in := registry.WithdrawalLimitInput{
		Asset: strings.ToUpper(asset), KYCLevel: int16(level), //nolint:gosec // bounded just above
		AutoApproveLimit: f.amount("auto_approve_limit"), DailyLimit: f.amount("daily_limit"),
		RequireManualReview: f.boolean("require_manual_review"),
	}
	if f.problem != "" {
		u.bounce(w, r, "/admin/withdrawal-limits", f.problem)
		return
	}
	_, err := u.h.upsertWithdrawalLimit(r.Context(), in, f.reason())
	u.done(w, r, "/admin/withdrawal-limits", err, "Limits for "+in.Asset+" at level "+strconv.Itoa(int(in.KYCLevel))+" saved.")
}

// form reads a posted form field by field and keeps the first complaint --
// a sentence for the person, not an error -- so a handler builds its input
// in one expression and checks once.
type form struct {
	r       *http.Request
	problem string
}

func newForm(r *http.Request) *form {
	f := &form{r: r}
	if err := r.ParseForm(); err != nil {
		f.problem = "Bad form."
	}
	return f
}

func (f *form) fail(msg string) {
	if f.problem == "" {
		f.problem = msg
	}
}

func (f *form) str(name string) string { return strings.TrimSpace(f.r.PostFormValue(name)) }

func (f *form) optStr(name string) *string {
	s := f.str(name)
	if s == "" {
		return nil
	}
	return &s
}

// reason is required on every write: it goes in the audit trail.
func (f *form) reason() string {
	s := f.str("reason")
	if s == "" {
		f.fail("A reason is required; it goes in the audit trail.")
	}
	return s
}

func (f *form) boolean(name string) bool {
	switch f.str(name) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

func (f *form) i32(name string) int32 {
	n, err := strconv.ParseInt(f.str(name), 10, 32)
	if err != nil {
		f.fail(name + " must be a whole number.")
	}
	return int32(n)
}

func (f *form) optI32(name string) *int32 {
	if f.str(name) == "" {
		return nil
	}
	n := f.i32(name)
	return &n
}

func (f *form) i64(name string) int64 {
	n, err := strconv.ParseInt(f.str(name), 10, 64)
	if err != nil {
		f.fail(name + " must be a whole number.")
	}
	return n
}

func (f *form) amount(name string) money.Amount {
	a, err := money.ParseAmount(f.str(name))
	if err != nil {
		f.fail(name + " must be a decimal amount.")
	}
	return a
}

func (f *form) optAmount(name string) *money.Amount {
	if f.str(name) == "" {
		return nil
	}
	a := f.amount(name)
	return &a
}
