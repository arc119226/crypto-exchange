package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// newE2ECmd is the Phase 3 demo of docs/plan-v1.0.md §12: two fresh users,
// dev-faucet funding through the admin API, the §6.1.4 worked example
// through the public API, a market order, and the invariants at the end.
// It exits non-zero on the first mismatch.
func newE2ECmd() *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use: "e2e", Short: "Register two users, fund them, trade the plan's worked example and check every balance", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runE2E(cmd, verbose)
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "print every step")
	c.Flags().String("admin-url", envOr("EXCHANGE_ADMIN_URL", "http://localhost:8082"), "admin API base URL (dev faucet)")
	c.Flags().String("admin-key", envOr("EXCHANGE_ADMIN_API_KEY", ""), "admin API key (X-Admin-Api-Key); env EXCHANGE_ADMIN_API_KEY")
	return c
}

type e2eUser struct {
	email   string
	session apiclient.Session
	client  *apiclient.ClientWithResponses
}

func runE2E(cmd *cobra.Command, verbose bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	step := func(format string, args ...any) {
		if verbose {
			_, _ = fmt.Fprintf(out, "• "+format+"\n", args...)
		}
	}
	base, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return err
	}
	adminURL, _ := cmd.Flags().GetString("admin-url")
	adminKey, _ := cmd.Flags().GetString("admin-key")
	if adminKey == "" {
		return fmt.Errorf("admin API key required for the dev faucet: --admin-key or EXCHANGE_ADMIN_API_KEY (see .env ADMIN_API_KEY)")
	}
	admin, err := adminclient.NewClientWithResponses(adminURL,
		adminclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		adminclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-Admin-Api-Key", adminKey)
			req.Header.Set(telemetry.RequestIDHeader, newRequestID())
			return nil
		}))
	if err != nil {
		return err
	}

	// 1. two users
	stamp := time.Now().UTC().Format("20060102t150405")
	newUser := func(name string) (*e2eUser, error) {
		u := &e2eUser{email: fmt.Sprintf("%s-%s-%s@e2e.local", name, stamp, newRequestID()[4:10])}
		anon, err := apiclient.NewClientWithResponses(base, apiclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}))
		if err != nil {
			return nil, err
		}
		resp, err := anon.RegisterWithResponse(ctx, apiclient.RegisterJSONRequestBody{Email: openapi_types.Email(u.email), Password: "e2e-password-" + stamp})
		if err != nil {
			return nil, transportError(base, err)
		}
		if resp.JSON201 == nil {
			return nil, fmt.Errorf("register %s: %w", name, problemError(resp.HTTPResponse, resp.Body))
		}
		u.session = *resp.JSON201
		creds := credentials{token: u.session.AccessToken}
		u.client, err = apiclient.NewClientWithResponses(base, apiclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}), apiclient.WithRequestEditorFn(creds.editor()))
		if err != nil {
			return nil, err
		}
		step("registered %s (%s) → account %s", name, u.email, u.session.AccountID)
		return u, nil
	}
	buyer, err := newUser("buyer")
	if err != nil {
		return err
	}
	seller, err := newUser("seller")
	if err != nil {
		return err
	}

	// 2. dev faucet (docs/plan-v1.0.md §6.1.4 g)
	fund := func(u *e2eUser, asset, amount string) error {
		amt, _ := money.ParseAmount(amount)
		resp, err := admin.CreateAdjustmentWithResponse(ctx, adminclient.CreateAdjustmentJSONRequestBody{
			AccountID: u.session.AccountID, Asset: asset, Amount: amt, Direction: adminclient.AdjustmentRequestDirectionCredit,
			Reason: "e2e faucet", IdempotencyKey: fmt.Sprintf("e2e:%s:%s:%s", stamp, u.session.AccountID, asset),
		})
		if err != nil {
			return transportError(adminURL, err)
		}
		if resp.JSON201 == nil && resp.JSON200 == nil {
			return fmt.Errorf("fund %s %s: %w", asset, amount, problemError(resp.HTTPResponse, resp.Body))
		}
		step("funded %s with %s %s", u.email, amount, asset)
		return nil
	}
	if err := fund(buyer, "USDC", "10000"); err != nil {
		return err
	}
	if err := fund(seller, "ETH", "1"); err != nil {
		return err
	}

	// 3. the worked example: S sells 0.4 @ 1990, B buys 1.0 @ 2000
	price1990, qty04 := money.MustParse("1990"), money.MustParse("0.4")
	s1, err := place(ctx, seller, apiclient.PlaceOrderJSONRequestBody{ClientOrderID: "e2e-s1-" + stamp, Market: "ETH-USDC", Side: "sell", Type: "limit", Price: &price1990, Qty: &qty04})
	if err != nil {
		return err
	}
	if err := expect("seller order status", string(s1.Order.Status), "open"); err != nil {
		return err
	}
	step("seller resting 0.4 ETH @ 1990 (%s)", s1.Order.ID)
	price2000, qty1 := money.MustParse("2000"), money.MustParse("1")
	b1, err := place(ctx, buyer, apiclient.PlaceOrderJSONRequestBody{ClientOrderID: "e2e-b1-" + stamp, Market: "ETH-USDC", Side: "buy", Type: "limit", Price: &price2000, Qty: &qty1})
	if err != nil {
		return err
	}
	if err := expect("buyer order status", string(b1.Order.Status), "partially_filled"); err != nil {
		return err
	}
	if len(b1.Trades) != 1 {
		return fmt.Errorf("buyer order: want 1 trade, got %d", len(b1.Trades))
	}
	if err := expectAmount("trade price", b1.Trades[0].Price, "1990"); err != nil {
		return err
	}
	if err := expectAmount("buyer hold remaining", b1.Order.HoldRemaining, "1200"); err != nil {
		return err
	}
	step("buyer filled 0.4 @ 1990, resting 0.6 @ 2000 (%s)", b1.Order.ID)

	// 4. balances exactly as §6.1.4 (b)
	if err := expectBalances(ctx, buyer, map[string][2]string{"USDC": {"8004", "1200"}, "ETH": {"0.3992", "0"}}); err != nil {
		return err
	}
	if err := expectBalances(ctx, seller, map[string][2]string{"USDC": {"795.204", "0"}, "ETH": {"0.6", "0"}}); err != nil {
		return err
	}
	step("balances match docs/plan-v1.0.md §6.1.4 (b)")

	// 5. replaying the same client_order_id is a 200 with the same order
	replay, err := buyer.client.PlaceOrderWithResponse(ctx, apiclient.PlaceOrderJSONRequestBody{ClientOrderID: "e2e-b1-" + stamp, Market: "ETH-USDC", Side: "buy", Type: "limit", Price: &price2000, Qty: &qty1})
	if err != nil {
		return transportError(base, err)
	}
	if replay.JSON200 == nil || replay.JSON200.Order.ID != b1.Order.ID {
		return fmt.Errorf("client_order_id replay: want HTTP 200 with order %s, got %d", b1.Order.ID, replay.StatusCode())
	}
	step("client_order_id replay → 200, same order")

	// 6. cancel the remainder (c): 1200 USDC released
	cancelled, err := buyer.client.CancelOrderWithResponse(ctx, b1.Order.ID)
	if err != nil {
		return transportError(base, err)
	}
	if cancelled.JSON200 == nil {
		return problemError(cancelled.HTTPResponse, cancelled.Body)
	}
	if err := expect("cancelled status", string(cancelled.JSON200.Status), "cancelled"); err != nil {
		return err
	}
	if err := expectBalances(ctx, buyer, map[string][2]string{"USDC": {"9204", "0"}}); err != nil {
		return err
	}
	step("cancel released 1200 USDC → available 9204")

	// 7. a market buy against a fresh ask
	price2010, qty05 := money.MustParse("2010"), money.MustParse("0.5")
	if _, err := place(ctx, seller, apiclient.PlaceOrderJSONRequestBody{ClientOrderID: "e2e-s2-" + stamp, Market: "ETH-USDC", Side: "sell", Type: "limit", Price: &price2010, Qty: &qty05}); err != nil {
		return err
	}
	budget := money.MustParse("1005")
	mb, err := place(ctx, buyer, apiclient.PlaceOrderJSONRequestBody{ClientOrderID: "e2e-m1-" + stamp, Market: "ETH-USDC", Side: "buy", Type: "market", QuoteQty: &budget})
	if err != nil {
		return err
	}
	if err := expect("market buy status", string(mb.Order.Status), "filled"); err != nil {
		return err
	}
	if err := expectAmount("market buy filled qty", mb.Order.FilledQty, "0.5"); err != nil {
		return err
	}
	step("market buy 1005 USDC filled 0.5 ETH @ 2010")

	// 8. the book shows nothing left from us; fills and the trial balance agree
	depth, err := buyer.client.GetDepthWithResponse(ctx, "ETH-USDC", &apiclient.GetDepthParams{})
	if err != nil {
		return transportError(base, err)
	}
	if depth.JSON200 == nil {
		return problemError(depth.HTTPResponse, depth.Body)
	}
	fills, err := buyer.client.ListFillsWithResponse(ctx, &apiclient.ListFillsParams{})
	if err != nil {
		return transportError(base, err)
	}
	if fills.JSON200 == nil || len(fills.JSON200.Fills) != 2 {
		return fmt.Errorf("buyer fills: want 2, got %v", fills.StatusCode())
	}
	tb, err := admin.GetTrialBalanceWithResponse(ctx)
	if err != nil {
		return transportError(adminURL, err)
	}
	if tb.JSON200 == nil {
		return problemError(tb.HTTPResponse, tb.Body)
	}
	if !tb.JSON200.Balanced {
		b, _ := json.Marshal(tb.JSON200)
		return fmt.Errorf("trial balance is not zero: %s", b)
	}
	step("trial balance is zero across %d assets", len(tb.JSON200.Lines))

	_, err = fmt.Fprintf(out, "e2e OK: buyer=%s seller=%s trades=2 (limit 0.4 @ 1990, market 0.5 @ 2010), balances and trial balance verified\n", buyer.session.AccountID, seller.session.AccountID)
	return err
}

func place(ctx context.Context, u *e2eUser, body apiclient.PlaceOrderJSONRequestBody) (*apiclient.OrderResult, error) {
	resp, err := u.client.PlaceOrderWithResponse(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("place %s: %w", body.ClientOrderID, err)
	}
	if resp.JSON201 == nil {
		return nil, fmt.Errorf("place %s: %w", body.ClientOrderID, problemError(resp.HTTPResponse, resp.Body))
	}
	if resp.JSON201.Order.Status == "rejected" {
		return nil, fmt.Errorf("place %s: rejected: %s", body.ClientOrderID, resp.JSON201.Order.RejectReason)
	}
	return resp.JSON201, nil
}

func expect(what, got, want string) error {
	if got != want {
		return fmt.Errorf("%s: want %s, got %s", what, want, got)
	}
	return nil
}

func expectAmount(what string, got money.Amount, want string) error {
	if !got.Equal(money.MustParse(want)) {
		return fmt.Errorf("%s: want %s, got %s", what, want, got)
	}
	return nil
}

// expectBalances checks available/hold per asset.
func expectBalances(ctx context.Context, u *e2eUser, want map[string][2]string) error {
	resp, err := u.client.ListBalancesWithResponse(ctx)
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return problemError(resp.HTTPResponse, resp.Body)
	}
	got := map[string]apiclient.Balance{}
	for _, b := range resp.JSON200.Balances {
		got[b.Asset] = b
	}
	for asset, w := range want {
		b, ok := got[asset]
		if !ok {
			return fmt.Errorf("%s: no %s balance", u.email, asset)
		}
		if err := expectAmount(u.email+" "+asset+" available", b.Available, w[0]); err != nil {
			return err
		}
		if err := expectAmount(u.email+" "+asset+" hold", b.Hold, w[1]); err != nil {
			return err
		}
	}
	return nil
}
