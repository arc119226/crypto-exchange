package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// Limits are the token-bucket limits the handlers apply (docs/plan-v1.0.md §14).
type Limits struct {
	LoginPerIP       ratelimit.Limit // also applied to registration
	LoginPerAccount  ratelimit.Limit
	OrdersPerAccount ratelimit.Limit
}

// Deps are the collaborators of the public API. Nil optional members switch
// the corresponding endpoints off with 503 (a role that runs without them,
// or a split deployment whose command bus arrives in a later step).
type Deps struct {
	Tenant   string
	Registry registry.Reader // required
	Auth     *auth.Service   // sessions, API keys, JWKS
	Ledger   *ledger.Service // balances, entries
	Trading  *trading.Service
	// Chain assigns deposit addresses; nil switches GET /v1/deposit-address
	// off with 503. It never derives — the signer role owns the keys.
	Chain *chain.Addresses
	// Deposits reads what the chain role recorded; nil switches
	// GET /v1/deposits off with 503.
	Deposits *deposit.Reader
	// Withdrawals records requests; nil switches /v1/withdrawals off with
	// 503. It never locks funds or signs — the chain role does both.
	Withdrawals *withdrawal.Service
	Limiter     ratelimit.Limiter // nil = unlimited (tests)
	Limits      Limits
}

// Handler implements gen.StrictServerInterface for one tenant.
type Handler struct {
	d Deps
}

var _ gen.StrictServerInterface = (*Handler)(nil)

// NewHandler builds the public API handler.
func NewHandler(d Deps) *Handler {
	if d.Tenant == "" {
		d.Tenant = "default"
	}
	return &Handler{d: d}
}

// --- request context ---

type reqInfoKey struct{}

type requestInfo struct {
	ip   string
	path string
}

// withRequestInfo is a strict-server middleware that keeps what handlers
// need from the raw request (client address, path) without giving them the
// request itself.
func withRequestInfo(f gen.StrictHandlerFunc, _ string) gen.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		return f(context.WithValue(ctx, reqInfoKey{}, requestInfo{ip: auth.ClientIP(r), path: r.URL.Path}), w, r, req)
	}
}

func reqInfo(ctx context.Context) requestInfo {
	ri, _ := ctx.Value(reqInfoKey{}).(requestInfo)
	return ri
}

// principal returns the caller or an ErrUnauthenticated.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, errUnauthenticated
	}
	return p, nil
}

// requireScope is principal plus a scope check (JWT sessions hold every scope).
func requireScope(ctx context.Context, scope auth.Scope) (auth.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if !p.Has(scope) {
		return p, fmt.Errorf("%w: scope %s required", errForbidden, scope)
	}
	return p, nil
}

var (
	errUnauthenticated = errors.New("api: authentication required")
	errForbidden       = errors.New("api: forbidden")
	errUnavailable     = errors.New("api: not available in this role")
)

// allow consults the limiter; nil limiter allows everything.
func (h *Handler) allow(ctx context.Context, key string, l ratelimit.Limit) (ratelimit.Decision, error) {
	if h.d.Limiter == nil || l.N <= 0 {
		return ratelimit.Decision{Allowed: true}, nil
	}
	return h.d.Limiter.Allow(ctx, key, l)
}

// --- problem helpers (one per status the spec uses) ---

func (h *Handler) problem(ctx context.Context, status int, title, detail string) gen.Problem {
	return NewProblem(ctx, status, title, detail, reqInfo(ctx).path)
}

func (h *Handler) badRequest(ctx context.Context, detail string) gen.BadRequestApplicationProblemPlusJSONResponse {
	return gen.BadRequestApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusBadRequest, "Bad Request", detail))
}

func (h *Handler) unauthorized(ctx context.Context) gen.UnauthorizedApplicationProblemPlusJSONResponse {
	return gen.UnauthorizedApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusUnauthorized, "Unauthorized", "authentication required: send a Bearer access token or a signed API key request"))
}

func (h *Handler) forbidden(ctx context.Context, detail string) gen.ForbiddenApplicationProblemPlusJSONResponse {
	return gen.ForbiddenApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusForbidden, "Forbidden", detail))
}

func (h *Handler) notFound(ctx context.Context, detail string) gen.NotFoundApplicationProblemPlusJSONResponse {
	return gen.NotFoundApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusNotFound, "Not Found", detail))
}

func (h *Handler) conflict(ctx context.Context, detail string) gen.ConflictApplicationProblemPlusJSONResponse {
	return gen.ConflictApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusConflict, "Conflict", detail))
}

func (h *Handler) unprocessable(ctx context.Context, detail string) gen.UnprocessableEntityApplicationProblemPlusJSONResponse {
	return gen.UnprocessableEntityApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusUnprocessableEntity, "Unprocessable Entity", detail))
}

func (h *Handler) tooMany(ctx context.Context, d ratelimit.Decision) gen.TooManyRequestsApplicationProblemPlusJSONResponse {
	secs := int(d.RetryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	return gen.TooManyRequestsApplicationProblemPlusJSONResponse{
		Body:    h.problem(ctx, http.StatusTooManyRequests, "Too Many Requests", fmt.Sprintf("rate limit exceeded; retry in %d s", secs)),
		Headers: gen.TooManyRequestsResponseHeaders{RetryAfter: &secs},
	}
}

func (h *Handler) unavailable(ctx context.Context, detail string) gen.ServiceUnavailableApplicationProblemPlusJSONResponse {
	return gen.ServiceUnavailableApplicationProblemPlusJSONResponse(h.problem(ctx, http.StatusServiceUnavailable, "Service Unavailable", detail))
}

// --- registry (unchanged since Phase 0) ---

// ListAssets implements GET /v1/assets.
func (h *Handler) ListAssets(ctx context.Context, _ gen.ListAssetsRequestObject) (gen.ListAssetsResponseObject, error) {
	assets, err := h.d.Registry.ListAssets(ctx, h.d.Tenant)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	out := make([]gen.Asset, 0, len(assets))
	for _, a := range assets {
		out = append(out, toAsset(a))
	}
	return gen.ListAssets200JSONResponse(gen.AssetList{Assets: out}), nil
}

// ListMarkets implements GET /v1/markets.
func (h *Handler) ListMarkets(ctx context.Context, _ gen.ListMarketsRequestObject) (gen.ListMarketsResponseObject, error) {
	markets, err := h.d.Registry.ListMarkets(ctx, h.d.Tenant)
	if err != nil {
		return nil, fmt.Errorf("list markets: %w", err)
	}
	out := make([]gen.Market, 0, len(markets))
	for _, m := range markets {
		if m.Status == registry.MarketDelisted {
			continue
		}
		out = append(out, toMarket(m))
	}
	return gen.ListMarkets200JSONResponse(gen.MarketList{Markets: out}), nil
}

// GetMarket implements GET /v1/markets/{symbol}.
func (h *Handler) GetMarket(ctx context.Context, req gen.GetMarketRequestObject) (gen.GetMarketResponseObject, error) {
	m, err := h.d.Registry.GetMarket(ctx, h.d.Tenant, req.Symbol)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && m.Status == registry.MarketDelisted) {
		return gen.GetMarket404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "market "+req.Symbol+" does not exist")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get market %s: %w", req.Symbol, err)
	}
	return gen.GetMarket200JSONResponse(toMarket(m)), nil
}

func toAsset(a registry.Asset) gen.Asset {
	return gen.Asset{
		Symbol:                a.Symbol,
		Name:                  a.Name,
		ChainID:               a.ChainID,
		ContractAddress:       a.ContractAddress,
		IsNative:              a.IsNative,
		Scale:                 a.Scale,
		DisplayScale:          a.DisplayScale,
		RequiredConfirmations: a.RequiredConfirmations,
		MinDeposit:            a.MinDeposit,
		MinWithdrawal:         a.MinWithdrawal,
		WithdrawalFee:         a.WithdrawalFee,
		DepositEnabled:        a.DepositEnabled,
		WithdrawEnabled:       a.WithdrawEnabled,
		Status:                gen.AssetStatus(a.Status),
	}
}

func toMarket(m registry.Market) gen.Market {
	return gen.Market{
		Symbol:          m.Symbol,
		BaseAsset:       m.BaseSymbol,
		QuoteAsset:      m.QuoteSymbol,
		PriceTick:       m.PriceTick,
		QtyStep:         m.QtyStep,
		MinNotional:     m.MinNotional,
		MaxQty:          m.MaxQty,
		MaxSlippageBps:  m.MaxSlippageBps,
		MakerBps:        m.MakerBps,
		TakerBps:        m.TakerBps,
		SelfTradePolicy: gen.SelfTradePolicy(m.SelfTradePolicy),
		Status:          gen.MarketStatus(m.Status),
	}
}
