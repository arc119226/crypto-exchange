//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/app"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

const fixtures = "../fixtures/addresses.dev.json"

func TestMigrateSeedAndRegistry(t *testing.T) {
	h := startPostgres(t)
	ctx := context.Background()

	// migrate twice: second run is a no-op
	var out bytes.Buffer
	require.NoError(t, app.MigrateUp(ctx, h.DSN("ex_migrate"), &out))
	assert.Contains(t, out.String(), "0001_bootstrap_schemas.sql")
	assert.Contains(t, out.String(), "0002_registry_core.sql")
	out.Reset()
	require.NoError(t, app.MigrateUp(ctx, h.DSN("ex_migrate"), &out))
	assert.Contains(t, out.String(), "no pending migrations")
	out.Reset()
	require.NoError(t, app.MigrateStatus(ctx, h.DSN("ex_migrate"), &out))
	assert.Equal(t, 11, strings.Count(out.String(), "applied"), out.String()) // 0001 schemas, 0002 registry, 0003 ledger, 0004 audit, 0005 trading, 0006 eventbus, 0007 auth, 0008 chain addresses, 0009 chain deposits, 0010 chain withdrawals, 0011 chain signing

	// seed twice with the admin role: idempotent, versions stay at 1
	seedOpts := app.SeedOptions{DSN: h.DSN("ex_admin"), FixturesPath: fixtures, TenantID: "default", ChainID: 31337, RequiredConfirmations: 1}
	require.NoError(t, app.Seed(ctx, seedOpts, &out))
	assert.Contains(t, out.String(), "markets=1")
	assert.Contains(t, out.String(), "withdrawal_limits=6")
	require.NoError(t, app.Seed(ctx, seedOpts, &out))

	// wrong chain id is refused before touching the database
	bad := seedOpts
	bad.ChainID = 11155111
	assert.ErrorIs(t, app.Seed(ctx, bad, &out), registry.ErrInvalid)

	// read back with the api role
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 2})
	require.NoError(t, err)
	defer pool.Close()
	store := registry.NewStore(pool)

	markets, err := store.ListMarkets(ctx, "default")
	require.NoError(t, err)
	require.Len(t, markets, 1)
	m := markets[0]
	assert.Equal(t, "ETH-USDC", m.Symbol)
	assert.Equal(t, "0.01", m.PriceTick.String())
	assert.Equal(t, "0.0001", m.QtyStep.String())
	assert.Equal(t, "5", m.MinNotional.String())
	assert.Nil(t, m.MaxQty)
	assert.Equal(t, int32(10), m.MakerBps)
	assert.Equal(t, int32(20), m.TakerBps)
	assert.Equal(t, int32(18), m.BaseScale)
	assert.Equal(t, int32(6), m.QuoteScale)
	assert.Equal(t, registry.STPCancelNewest, m.SelfTradePolicy)
	assert.Equal(t, registry.MarketActive, m.Status)
	assert.Equal(t, int32(1), m.Version, "second seed must not bump the version")

	// withdrawal limits: three KYC levels per asset, and the api role can read
	// them because the policy check needs them (migration 0002 grants SELECT on
	// the whole registry schema).
	limits, err := store.ListWithdrawalLimits(ctx, "default")
	require.NoError(t, err)
	require.Len(t, limits, 6)
	eth0, err := store.GetWithdrawalLimit(ctx, "default", "ETH", 0)
	require.NoError(t, err)
	assert.Equal(t, "0.1", eth0.AutoApproveLimit.String())
	assert.Equal(t, "1", eth0.DailyLimit.String())
	assert.False(t, eth0.RequireManualReview)
	assert.Equal(t, int32(1), eth0.Version, "second seed must not bump the version")
	_, err = store.GetWithdrawalLimit(ctx, "default", "ETH", 3)
	assert.ErrorIs(t, err, registry.ErrNotFound, "an unknown kyc level has no limit, it is not silently unlimited")

	got, err := store.GetMarket(ctx, "default", "ETH-USDC")
	require.NoError(t, err)
	assert.Equal(t, m.ID, got.ID)
	_, err = store.GetMarket(ctx, "default", "NOPE-USDC")
	assert.ErrorIs(t, err, registry.ErrNotFound)
	_, err = store.GetMarket(ctx, "other-tenant", "ETH-USDC")
	assert.ErrorIs(t, err, registry.ErrNotFound)

	assets, err := store.ListAssets(ctx, "default")
	require.NoError(t, err)
	require.Len(t, assets, 2)
	assert.Equal(t, "ETH", assets[0].Symbol)
	assert.True(t, assets[0].IsNative)
	assert.Nil(t, assets[0].ContractAddress)
	assert.Equal(t, "USDC", assets[1].Symbol)
	require.NotNil(t, assets[1].ContractAddress)
	assert.Equal(t, "0x5FbDB2315678afecb367f032d93F642f64180aa3", *assets[1].ContractAddress)
	assert.Equal(t, int32(1), assets[1].Version)

	cache := registry.NewCache("default")
	require.NoError(t, cache.Load(ctx, store))
	_, ok := cache.Market("ETH-USDC")
	assert.True(t, ok)

	// the api role must not be able to write the registry
	_, err = pool.Exec(ctx, `INSERT INTO registry.fee_schedules (tenant_id, name, maker_bps, taker_bps) VALUES ('default', 'hack', 0, 0)`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42501", pgErr.Code, "insufficient_privilege expected")

	// the walking skeleton end to end: HTTP → OpenAPI handler → registry (ex_api) → Postgres
	router := chi.NewRouter()
	router.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	api.Mount(router, api.NewHandler(api.Deps{Tenant: "default", Registry: store}))
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, body := httpGet(t, srv.URL+"/v1/markets", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(body), `"price_tick":"0.01"`, "Phase 0 DoD: amounts are JSON strings")
	var list gen.MarketList
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Markets, 1)
	assert.Equal(t, "ETH-USDC", list.Markets[0].Symbol)
	assert.Equal(t, "ETH", list.Markets[0].BaseAsset)
	assert.Equal(t, "USDC", list.Markets[0].QuoteAsset)
	assert.Equal(t, int32(20), list.Markets[0].TakerBps)

	resp, body = httpGet(t, srv.URL+"/v1/assets", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var assetList gen.AssetList
	require.NoError(t, json.Unmarshal(body, &assetList))
	require.Len(t, assetList.Assets, 2)
	assert.Nil(t, assetList.Assets[0].ContractAddress)
	require.NotNil(t, assetList.Assets[1].ContractAddress)

	resp, body = httpGet(t, srv.URL+"/v1/markets/NOPE-USDC", "it-corr-42")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, api.ProblemContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "it-corr-42", resp.Header.Get(telemetry.RequestIDHeader))
	var problem gen.Problem
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, "it-corr-42", problem.CorrelationID)
	assert.Equal(t, http.StatusNotFound, problem.Status)
	assert.Equal(t, "/v1/markets/NOPE-USDC", problem.Instance)
}

func httpGet(t *testing.T, url, requestID string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if requestID != "" {
		req.Header.Set(telemetry.RequestIDHeader, requestID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func TestNumericRoundTripAgainstPostgres(t *testing.T) {
	h := startPostgres(t)
	ctx := context.Background()
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("exchange"), MaxConns: 2})
	require.NoError(t, err)
	defer pool.Close()

	for _, s := range []string{"0", "1", "-1", "1990.5", "1990.00", "0.000000000000000001", "123456789012345678.123456789012345678", "-0.5"} {
		a := money.MustParse(s)
		var n pgtype.Numeric
		require.NoError(t, pool.QueryRow(ctx, `SELECT $1::numeric(36,18)`, pg.NumericFromAmount(a)).Scan(&n), s)
		back, err := pg.AmountFromNumeric(n)
		require.NoError(t, err, s)
		assert.True(t, a.Equal(back), "%s round-tripped as %s (Int=%v Exp=%d)", s, back, n.Int, n.Exp)
	}

	// 2^256-1 does not fit NUMERIC(36,18); Postgres must reject it.
	max256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	var n pgtype.Numeric
	err = pool.QueryRow(ctx, `SELECT $1::numeric(36,18)`, pgtype.Numeric{Int: max256, Valid: true}).Scan(&n)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		assert.Equal(t, "22003", pgErr.Code, "numeric_value_out_of_range")
	}
}
