package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// newOrdersCmd groups the trading commands (docs/plan-v1.0.md §7.4 trading group).
func newOrdersCmd() *cobra.Command {
	c := &cobra.Command{Use: "orders", Short: "Place, cancel and list orders (/v1/orders)"}
	c.AddCommand(newOrdersPlaceCmd(), newOrdersCancelCmd(), newOrdersListCmd(), newOrdersGetCmd())
	return c
}

type placeFlags struct {
	market, side, typ, tif, price, qty, quoteQty, clientOrderID string
}

func (f *placeFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.market, "market", "ETH-USDC", "market symbol")
	c.Flags().StringVar(&f.side, "side", "", "buy|sell")
	c.Flags().StringVar(&f.typ, "type", "limit", "limit|market")
	c.Flags().StringVar(&f.tif, "tif", "", "gtc|ioc (limit orders; default gtc)")
	c.Flags().StringVar(&f.price, "price", "", "limit price")
	c.Flags().StringVar(&f.qty, "qty", "", "base quantity (limit orders, market sells)")
	c.Flags().StringVar(&f.quoteQty, "quote-qty", "", "quote budget (market buys)")
	c.Flags().StringVar(&f.clientOrderID, "client-order-id", "", "idempotency key (default: random)")
	_ = c.MarkFlagRequired("side")
}

func (f placeFlags) body() (apiclient.PlaceOrderJSONRequestBody, error) {
	b := apiclient.PlaceOrderJSONRequestBody{
		ClientOrderID: f.clientOrderID, Market: f.market, Side: apiclient.Side(f.side), Type: apiclient.OrderType(f.typ),
	}
	if b.ClientOrderID == "" {
		b.ClientOrderID = "ctl-" + newRequestID()[4:]
	}
	if f.tif != "" {
		tif := apiclient.TimeInForce(f.tif)
		b.TimeInForce = &tif
	}
	set := func(dst **apiclient.Amount, raw, name string) error {
		if raw == "" {
			return nil
		}
		a, err := money.ParseAmount(raw)
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		*dst = &a
		return nil
	}
	if err := set(&b.Price, f.price, "price"); err != nil {
		return b, err
	}
	if err := set(&b.Qty, f.qty, "qty"); err != nil {
		return b, err
	}
	if err := set(&b.QuoteQty, f.quoteQty, "quote-qty"); err != nil {
		return b, err
	}
	return b, nil
}

func newOrdersPlaceCmd() *cobra.Command {
	var f placeFlags
	c := &cobra.Command{
		Use: "place", Short: "Place an order (POST /v1/orders)", Args: cobra.NoArgs,
		Example: `  exchangectl orders place --side sell --price 1990 --qty 0.4
  exchangectl orders place --side buy --type market --quote-qty 500`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			body, err := f.body()
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.PlaceOrderWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			res := resp.JSON201
			if res == nil {
				res = resp.JSON200 // replay of the same client_order_id
			}
			if res == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if err := printOrders(cmd, []apiclient.Order{res.Order}); err != nil {
				return err
			}
			if len(res.Trades) > 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
					return err
				}
				return printFills(cmd, res.Trades)
			}
			return nil
		},
	}
	f.bind(c)
	return c
}

func newOrdersCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use: "cancel <order-id>", Short: "Cancel an order (DELETE /v1/orders/{id})", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.CancelOrderWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printOrders(cmd, []apiclient.Order{*resp.JSON200})
		},
	}
}

func newOrdersListCmd() *cobra.Command {
	var market, status string
	var openOnly bool
	var limit int32
	c := &cobra.Command{
		Use: "list", Short: "List orders (GET /v1/orders)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			params := &apiclient.ListOrdersParams{Limit: &limit}
			if market != "" {
				params.Market = &market
			}
			if status != "" {
				st := apiclient.OrderStatus(status)
				params.Status = &st
			}
			if openOnly {
				params.OpenOnly = &openOnly
			}
			resp, err := client.ListOrdersWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printOrders(cmd, resp.JSON200.Orders)
		},
	}
	c.Flags().StringVar(&market, "market", "", "filter by market")
	c.Flags().StringVar(&status, "status", "", "filter by status")
	c.Flags().BoolVar(&openOnly, "open", false, "only open and partially filled orders")
	c.Flags().Int32Var(&limit, "limit", 50, "page size")
	return c
}

func newOrdersGetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "get <order-id>", Short: "Show an order and its fills (GET /v1/orders/{id})", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetOrderWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			if err := printOrders(cmd, []apiclient.Order{resp.JSON200.Order}); err != nil {
				return err
			}
			if len(resp.JSON200.Trades) > 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
					return err
				}
				return printFills(cmd, resp.JSON200.Trades)
			}
			return nil
		},
	}
}

func amountStr(a *apiclient.Amount) string {
	if a == nil {
		return "-"
	}
	return a.String()
}

func printOrders(cmd *cobra.Command, orders []apiclient.Order) error {
	rows := make([][]string, 0, len(orders))
	for _, o := range orders {
		reason := o.RejectReason
		if reason == "" {
			reason = o.CancelReason
		}
		if reason == "" {
			reason = "-"
		}
		seq := "-"
		if o.Seq != nil {
			seq = strconv.FormatInt(*o.Seq, 10)
		}
		rows = append(rows, []string{
			o.ID, o.ClientOrderID, o.Market, string(o.Side), string(o.Type), string(o.TimeInForce),
			amountStr(o.Price), amountStr(o.Qty), amountStr(o.QuoteQty), o.FilledQty.String(), o.RemainingQty.String(),
			o.HoldRemaining.String() + " " + o.HoldAsset, string(o.Status), reason, seq,
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"ID", "CLIENT_ID", "MARKET", "SIDE", "TYPE", "TIF", "PRICE", "QTY", "QUOTE_QTY", "FILLED", "REMAINING", "HOLD", "STATUS", "REASON", "SEQ"}, rows)
}

func printFills(cmd *cobra.Command, fills []apiclient.Fill) error {
	rows := make([][]string, 0, len(fills))
	for _, f := range fills {
		role := "taker"
		if f.IsMaker {
			role = "maker"
		}
		rows = append(rows, []string{f.TradeID, f.OrderID, f.Market, string(f.Side), role, f.Price.String(), f.Qty.String(), f.QuoteQty.String(), f.Fee.String() + " " + f.FeeAsset, strconv.FormatInt(f.Seq, 10)})
	}
	return printTable(cmd.OutOrStdout(), []string{"TRADE_ID", "ORDER_ID", "MARKET", "SIDE", "ROLE", "PRICE", "QTY", "QUOTE_QTY", "FEE", "SEQ"}, rows)
}
