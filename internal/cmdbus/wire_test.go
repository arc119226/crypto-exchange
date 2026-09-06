package cmdbus

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

func amt(s string) money.Amount { return money.MustParse(s) }

// TestErrorRoundTrip is the load-bearing test of this package: internal/api
// turns trading's sentinel errors into 404 / 422 / 503, so a command that
// crosses the bus must arrive at the api as the same sentinel. Losing this
// would silently turn every cross-container rejection into a 500.
func TestErrorRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		sentinel error
		wantKind Kind
	}{
		{"invalid request", trading.ErrInvalidRequest, KindInvalidRequest},
		{"market not found", trading.ErrMarketNotFound, KindMarketNotFound},
		{"order not found", trading.ErrOrderNotFound, KindOrderNotFound},
		{"client order id mismatch", trading.ErrClientOrderIDMismatch, KindClientOrderIDMismatch},
		{"engine unavailable", trading.ErrEngineUnavailable, KindUnavailable},
		{"rejected credential", ErrRejected, KindRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fmt.Errorf("%w: detail that must survive", tc.sentinel)
			wire := errorOf(in)
			require.Equal(t, tc.wantKind, wire.Kind)

			// the failure crosses as JSON and comes back as a local error
			b, err := json.Marshal(Response{Error: wire})
			require.NoError(t, err)
			var resp Response
			require.NoError(t, json.Unmarshal(b, &resp))
			require.NotNil(t, resp.Error)

			out := resp.Error.err()
			assert.ErrorIs(t, out, tc.sentinel, "the api must still recognise the sentinel")
			assert.Equal(t, in.Error(), out.Error(), "message preserved verbatim")
		})
	}

	t.Run("unknown errors are internal and match no sentinel", func(t *testing.T) {
		wire := errorOf(errors.New("boom"))
		assert.Equal(t, KindInternal, wire.Kind)
		out := wire.err()
		assert.Contains(t, out.Error(), "boom")
		for _, s := range []error{
			trading.ErrInvalidRequest, trading.ErrMarketNotFound, trading.ErrOrderNotFound,
			trading.ErrClientOrderIDMismatch, trading.ErrEngineUnavailable, ErrRejected,
		} {
			assert.NotErrorIs(t, out, s)
		}
	})

	t.Run("engine faults stay internal", func(t *testing.T) {
		// a sequence conflict or an inconsistent book is the engine's fault,
		// not the caller's: the api answers 500, never 4xx
		for _, err := range []error{trading.ErrSequenceConflict, trading.ErrBookInconsistent} {
			assert.Equal(t, KindInternal, errorOf(err).Kind, err)
		}
	})
}

// TestCommandRoundTrip pins the encoding of the trading types the bus sends
// as-is: amounts stay decimal strings and the enums stay readable.
func TestCommandRoundTrip(t *testing.T) {
	req := Request{
		Op: OpPlace, Market: "ETH-USDC", Token: "tok",
		Place: &trading.PlaceOrderRequest{
			AccountID: "acct-1", MarketSymbol: "ETH-USDC", ClientOrderID: "b1",
			Side: matching.Buy, Type: matching.Limit, Price: amt("2000"), Qty: amt("0.4"),
			CorrelationID: "corr-1",
		},
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"Price":"2000"`, "amounts are decimal strings, never JSON numbers")
	assert.Contains(t, string(b), `"Side":"buy"`)
	assert.Contains(t, string(b), `"Type":"limit"`)

	var got Request
	require.NoError(t, json.Unmarshal(b, &got))
	require.NotNil(t, got.Place)
	assert.Equal(t, req.Place.AccountID, got.Place.AccountID)
	assert.Equal(t, req.Place.ClientOrderID, got.Place.ClientOrderID)
	assert.Equal(t, req.Place.Side, got.Place.Side)
	assert.Equal(t, req.Place.Type, got.Place.Type)
	assert.True(t, req.Place.Price.Equal(got.Place.Price))
	assert.True(t, req.Place.Qty.Equal(got.Place.Qty))
	assert.Equal(t, req.Place.CorrelationID, got.Place.CorrelationID)
	assert.NoError(t, got.Place.Validate())
}

// TestTimeInForceCrossesUnchanged guards the one enum whose zero value is
// meaningful: an omitted time_in_force means GTC for a limit order and IOC
// for a market order. It decodes as GTC, which both Validate and the
// engine's effective-TIF rule treat identically to the zero value.
func TestTimeInForceCrossesUnchanged(t *testing.T) {
	cases := []struct {
		name string
		req  trading.PlaceOrderRequest
	}{
		{"limit", trading.PlaceOrderRequest{Type: matching.Limit, Side: matching.Buy, Price: amt("2000"), Qty: amt("1")}},
		{"market buy", trading.PlaceOrderRequest{Type: matching.Market, Side: matching.Buy, QuoteQty: amt("500")}},
		{"market sell", trading.PlaceOrderRequest{Type: matching.Market, Side: matching.Sell, Qty: amt("1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.AccountID, req.MarketSymbol, req.ClientOrderID = "a", "ETH-USDC", "c"
			require.Zero(t, req.TimeInForce)
			require.NoError(t, req.Validate())

			b, err := json.Marshal(req)
			require.NoError(t, err)
			var got trading.PlaceOrderRequest
			require.NoError(t, json.Unmarshal(b, &got))
			assert.Equal(t, matching.GTC, got.TimeInForce, "zero decodes to the documented default")
			assert.NoError(t, got.Validate(), "and stays a valid request")
		})
	}
}

func TestResponseShapes(t *testing.T) {
	depth := matching.Depth{Symbol: "ETH-USDC", LastSeq: 7, Bids: []matching.Level{{Price: amt("2000"), Qty: amt("0.6"), Orders: 1}}}
	b, err := json.Marshal(Response{Book: &depth})
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.Unmarshal(b, &resp))
	require.NotNil(t, resp.Book)
	assert.Nil(t, resp.Error)
	assert.Equal(t, uint64(7), resp.Book.LastSeq)
	require.Len(t, resp.Book.Bids, 1)
	assert.True(t, amt("0.6").Equal(resp.Book.Bids[0].Qty))
}

func TestSubject(t *testing.T) {
	c := &Client{cfg: Config{Tenant: "default"}.withDefaults()}
	assert.Equal(t, "cmd.trading.default.ETH-USDC", c.Subject("ETH-USDC"))

	c = &Client{cfg: Config{Tenant: "acme", SubjectPrefix: "x.cmd", Timeout: time.Second}.withDefaults()}
	assert.Equal(t, "x.cmd.acme.ETH-USDC", c.Subject("ETH-USDC"))
}

func TestNewClientRequiresConnection(t *testing.T) {
	_, err := NewClient(nil, Config{})
	assert.Error(t, err)
}

func TestStatusLabels(t *testing.T) {
	assert.Equal(t, "ok", status(nil))
	assert.Equal(t, "unavailable", status(fmt.Errorf("%w: x", trading.ErrEngineUnavailable)))
	assert.Equal(t, "market_not_found", status(fmt.Errorf("%w: x", trading.ErrMarketNotFound)))
	assert.Equal(t, "rejected", status(fmt.Errorf("%w: x", ErrRejected)))
	assert.Equal(t, "internal", status(errors.New("boom")))
}
