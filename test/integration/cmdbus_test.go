//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/cmdbus"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// busHarness is a trading harness whose commands travel over a real NATS
// server, the way api and engine talk in the compose app profile.
type busHarness struct {
	*tradingHarness
	nc     *nats.Conn
	signer *auth.Signer
	client *cmdbus.Client
	server *cmdbus.Server
	remote *trading.Service // an api-side service whose bus is the NATS client
}

func setupBus(t *testing.T) *busHarness {
	t.Helper()
	h := setupTrading(t)
	url := startNATS(t)
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = nc.Drain() })

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)

	server, err := cmdbus.Serve(nc, h.engine, cmdbus.ServerConfig{
		Tenant: "default", Verifier: verifier, Logger: h.log, Timeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	client, err := cmdbus.NewClient(nc, cmdbus.Config{
		Tenant: "default", Timeout: 10 * time.Second,
		Token: func(ctx context.Context) (string, error) { return tokenFrom(ctx, signer) },
	})
	require.NoError(t, err)

	b := &busHarness{tradingHarness: h, nc: nc, signer: signer, client: client, server: server}
	b.remote = trading.NewService(h.all, client, h.cache, "default")
	return b
}

// tokenFrom mints the internal token exactly as internal/app does: from the
// principal the api role authenticated.
func tokenFrom(ctx context.Context, signer *auth.Signer) (string, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return "", errors.New("no principal in context")
	}
	return signer.Issue(auth.Claims{
		UserID: p.UserID, AccountID: p.AccountID, TenantID: p.TenantID, Role: p.Role,
		Scopes: p.Scopes, Method: p.Method, Audience: auth.AudienceInternal,
	}, time.Now().UTC(), 5*time.Minute)
}

// as returns a context carrying the principal of an account, which is what
// the api's authentication middleware produces.
func as(account string, scopes ...auth.Scope) context.Context {
	if len(scopes) == 0 {
		scopes = []auth.Scope{auth.ScopeRead, auth.ScopeTrade}
	}
	return auth.WithPrincipal(context.Background(), auth.Principal{
		UserID: "user-" + account, AccountID: account, TenantID: "default",
		Role: auth.RoleUser, Method: auth.MethodJWT, Scopes: scopes,
	})
}

// TestCommandBusRoundTrip runs the worked example of docs/plan-v1.0.md
// §6.1.4 through NATS instead of a channel: the api side never touches the
// engine object, so every number here crossed the wire.
func TestCommandBusRoundTrip(t *testing.T) {
	h := setupBus(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "1"})

	sell, err := h.remote.PlaceOrder(as(seller), limit(seller, "s1", matching.Sell, "1990", "0.4"))
	require.NoError(t, err)
	assert.Equal(t, trading.StatusOpen, sell.Order.Status)
	eq(t, "0.4", sell.Order.HoldRemaining)

	buy, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "b1", matching.Buy, "2000", "1"))
	require.NoError(t, err)
	assert.Equal(t, trading.StatusPartiallyFilled, buy.Order.Status)
	eq(t, "0.4", buy.Order.FilledQty)
	eq(t, "796", buy.Order.FilledQuote)
	eq(t, "1200", buy.Order.HoldRemaining, "2000 - 796 - 4 price improvement")
	require.Len(t, buy.Trades, 1)
	eq(t, "1990", buy.Trades[0].Price)
	eq(t, "0.0008", buy.Trades[0].TakerFee)
	assert.Equal(t, "ETH", buy.Trades[0].TakerFeeAsset)
	require.NotNil(t, buy.Order.Seq)

	eq(t, "8004", h.balance(t, ctx, buyer, "USDC").Available)
	eq(t, "1200", h.balance(t, ctx, buyer, "USDC").Hold)
	eq(t, "0.3992", h.balance(t, ctx, buyer, "ETH").Available)
	eq(t, "795.204", h.balance(t, ctx, seller, "USDC").Available)

	t.Run("replay is idempotent across the bus", func(t *testing.T) {
		again, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "b1", matching.Buy, "2000", "1"))
		require.NoError(t, err)
		assert.True(t, again.Replayed)
		assert.Equal(t, buy.Order.ID, again.Order.ID)
	})

	t.Run("depth needs no credential", func(t *testing.T) {
		// depth is public data: the client sends no token for it
		book, err := h.client.Depth(context.Background(), market, 10)
		require.NoError(t, err)
		assert.Equal(t, market, book.Symbol)
		require.Len(t, book.Bids, 1)
		eq(t, "2000", book.Bids[0].Price)
		eq(t, "0.6", book.Bids[0].Qty)
	})

	t.Run("cancel releases the hold", func(t *testing.T) {
		order, err := h.remote.CancelOrder(as(buyer), trading.CancelRequest{AccountID: buyer, OrderID: buy.Order.ID})
		require.NoError(t, err)
		assert.Equal(t, trading.StatusCancelled, order.Status)
		eq(t, "9204", h.balance(t, ctx, buyer, "USDC").Available)
		eq(t, "0", h.balance(t, ctx, buyer, "USDC").Hold)
	})

	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
}

// TestCommandBusErrorMapping is the reason the wire carries an error kind:
// internal/api turns these sentinels into 404 / 422 / 503, and they have to
// survive the crossing.
func TestCommandBusErrorMapping(t *testing.T) {
	h := setupBus(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})

	// an order whose client_order_id the mismatch case below reuses
	_, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "dup", matching.Buy, "2000", "0.1"))
	require.NoError(t, err)

	t.Run("unknown market", func(t *testing.T) {
		req := limit(buyer, "nomarket", matching.Buy, "2000", "0.1")
		req.MarketSymbol = "NOPE-USDC"
		_, err := h.remote.PlaceOrder(as(buyer), req)
		assert.ErrorIs(t, err, trading.ErrMarketNotFound)
	})

	t.Run("client_order_id reused with different parameters", func(t *testing.T) {
		_, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "dup", matching.Buy, "2001", "0.1"))
		assert.ErrorIs(t, err, trading.ErrClientOrderIDMismatch)
	})

	t.Run("unknown order", func(t *testing.T) {
		_, err := h.remote.CancelOrder(as(buyer), trading.CancelRequest{AccountID: buyer, OrderID: "01J00000000000000000000000"})
		assert.ErrorIs(t, err, trading.ErrOrderNotFound)
	})

	t.Run("malformed request", func(t *testing.T) {
		bad := limit(buyer, "", matching.Buy, "2000", "0.1") // no client_order_id
		_, err := h.remote.PlaceOrder(as(buyer), bad)
		assert.ErrorIs(t, err, trading.ErrInvalidRequest)
	})

	t.Run("business rejections are orders, not errors", func(t *testing.T) {
		res, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "too-big", matching.Buy, "2000", "100"))
		require.NoError(t, err)
		assert.Equal(t, trading.StatusRejected, res.Order.Status)
		assert.Equal(t, trading.RejectInsufficientBalance, res.Order.RejectReason)
	})

	t.Run("no engine listening is unavailable, not an internal error", func(t *testing.T) {
		require.NoError(t, h.server.Close())
		_, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "after-stop", matching.Buy, "2000", "0.1"))
		assert.ErrorIs(t, err, trading.ErrEngineUnavailable, "the api answers 503, not 500")
		_, err = h.client.Depth(context.Background(), market, 5)
		assert.ErrorIs(t, err, trading.ErrEngineUnavailable)
	})

}

// TestCommandBusRejectsBadCredentials: the engine trusts the token, never
// the account id in the command body (docs/plan-v1.0.md §14).
func TestCommandBusRejectsBadCredentials(t *testing.T) {
	h := setupBus(t)
	ctx := context.Background()
	victim := h.account(t, ctx, map[string]string{"USDC": "10000"})
	attacker := h.account(t, ctx, map[string]string{"USDC": "1"})

	t.Run("a token for one account cannot move another", func(t *testing.T) {
		// authenticated as the attacker, but the command names the victim
		_, err := h.remote.PlaceOrder(as(attacker), limit(victim, "steal", matching.Buy, "2000", "1"))
		assert.ErrorIs(t, err, cmdbus.ErrRejected)
	})

	t.Run("a read-only token cannot trade", func(t *testing.T) {
		_, err := h.remote.PlaceOrder(as(victim, auth.ScopeRead), limit(victim, "readonly", matching.Buy, "2000", "0.1"))
		assert.ErrorIs(t, err, cmdbus.ErrRejected)
	})

	t.Run("no token at all", func(t *testing.T) {
		bare, err := cmdbus.NewClient(h.nc, cmdbus.Config{Tenant: "default", Timeout: 5 * time.Second})
		require.NoError(t, err)
		_, err = bare.PlaceOrder(context.Background(), limit(victim, "anon", matching.Buy, "2000", "0.1"))
		assert.ErrorIs(t, err, cmdbus.ErrRejected)
	})

	t.Run("an expired token", func(t *testing.T) {
		stale, err := cmdbus.NewClient(h.nc, cmdbus.Config{
			Tenant: "default", Timeout: 5 * time.Second,
			Token: func(context.Context) (string, error) {
				return h.signer.Issue(auth.Claims{
					UserID: "u", AccountID: victim, TenantID: "default", Role: auth.RoleUser,
					Scopes: []auth.Scope{auth.ScopeTrade}, Method: auth.MethodJWT, Audience: auth.AudienceInternal,
				}, time.Now().Add(-2*time.Hour), time.Minute)
			},
		})
		require.NoError(t, err)
		_, err = stale.PlaceOrder(context.Background(), limit(victim, "stale", matching.Buy, "2000", "0.1"))
		assert.ErrorIs(t, err, cmdbus.ErrRejected)
	})

	t.Run("a public-audience token is not an internal one", func(t *testing.T) {
		wrongAud, err := cmdbus.NewClient(h.nc, cmdbus.Config{
			Tenant: "default", Timeout: 5 * time.Second,
			Token: func(context.Context) (string, error) {
				return h.signer.Issue(auth.Claims{
					UserID: "u", AccountID: victim, TenantID: "default", Role: auth.RoleUser,
					Scopes: []auth.Scope{auth.ScopeTrade}, Method: auth.MethodJWT, Audience: auth.AudiencePublic,
				}, time.Now(), time.Minute)
			},
		})
		require.NoError(t, err)
		_, err = wrongAud.PlaceOrder(context.Background(), limit(victim, "aud", matching.Buy, "2000", "0.1"))
		assert.ErrorIs(t, err, cmdbus.ErrRejected)
	})

	// nothing above moved any money
	eq(t, "10000", h.balance(t, ctx, victim, "USDC").Available)
	eq(t, "0", h.balance(t, ctx, victim, "USDC").Hold)
	h.assertTrialBalanceZero(t, ctx)
}

// TestRegistryReloadHaltsTradingAcrossTheBus is the §6.6 loop end to end:
// the admin write commits, the relay publishes market.updated, the engine's
// consumer reloads, and the very next order is rejected — no restart.
func TestRegistryReloadHaltsTrading(t *testing.T) {
	h := setupBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})

	js, err := jetstream.New(h.nc)
	require.NoError(t, err)
	require.NoError(t, eventbus.EnsureStreams(ctx, js))
	stream, err := js.Stream(ctx, eventbus.StreamRegistry)
	require.NoError(t, err)
	require.NoError(t, stream.Purge(ctx), "a shared local server may hold events of earlier runs")

	reloaded := make(chan string, 8)
	sub, err := eventbus.Subscribe(ctx, js, eventbus.ConsumerConfig{
		Durable: "engine-registry-test", Stream: eventbus.StreamRegistry,
		FilterSubjects: []string{eventbus.SubjectPrefix + ".market.*.default.*"},
	}, h.log, func(ctx context.Context, e eventbus.Envelope) error {
		if err := h.engine.Reload(ctx); err != nil {
			return err
		}
		reloaded <- e.EventType
		return nil
	})
	require.NoError(t, err)
	defer sub.Stop()

	// before: the market trades
	ok, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "before", matching.Buy, "2000", "0.1"))
	require.NoError(t, err)
	assert.Equal(t, trading.StatusOpen, ok.Order.Status)

	// halt it through the store + outbox, exactly as the admin handler does
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		m, changed, err := h.store.SetMarketStatus(ctx, tx, "default", market, registry.MarketHalted)
		if err != nil {
			return err
		}
		require.True(t, changed)
		evt, err := registry.MarketUpdatedEvent("default", m, []string{"status"}, "incident", time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	}))

	relay := eventbus.NewRelay(h.all, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{}, h.log)
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = relay.Run(relayCtx) }()

	select {
	case et := <-reloaded:
		assert.Equal(t, registry.EventMarketUpdated, et)
	case <-time.After(30 * time.Second):
		t.Fatal("the engine never reloaded after market.updated")
	}

	// after: new orders are refused, and the refusal is a persisted order
	res, err := h.remote.PlaceOrder(as(buyer), limit(buyer, "after", matching.Buy, "2000", "0.1"))
	require.NoError(t, err)
	assert.Equal(t, trading.StatusRejected, res.Order.Status)
	assert.Equal(t, trading.RejectMarketNotActive, res.Order.RejectReason)

	// cancels still work on a halted market (docs/plan-v1.0.md §6.6)
	cancelled, err := h.remote.CancelOrder(as(buyer), trading.CancelRequest{AccountID: buyer, OrderID: ok.Order.ID})
	require.NoError(t, err)
	assert.Equal(t, trading.StatusCancelled, cancelled.Status)

	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
}
