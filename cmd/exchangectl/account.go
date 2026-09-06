package main

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func newBalancesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "balances", Short: "Balances of the authenticated account (GET /v1/balances)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListBalancesWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Balances))
			for _, b := range resp.JSON200.Balances {
				rows = append(rows, []string{b.Asset, b.Available.String(), b.Hold.String(), b.Total.String()})
			}
			return printTable(cmd.OutOrStdout(), []string{"ASSET", "AVAILABLE", "HOLD", "TOTAL"}, rows)
		},
	}
}

func newFillsCmd() *cobra.Command {
	var market, orderID string
	var limit int32
	c := &cobra.Command{
		Use: "fills", Short: "The authenticated account's fills (GET /v1/fills)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			params := &apiclient.ListFillsParams{Limit: &limit}
			if market != "" {
				params.Market = &market
			}
			if orderID != "" {
				params.OrderID = &orderID
			}
			resp, err := client.ListFillsWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printFills(cmd, resp.JSON200.Fills)
		},
	}
	c.Flags().StringVar(&market, "market", "", "filter by market")
	c.Flags().StringVar(&orderID, "order-id", "", "filter by order")
	c.Flags().Int32Var(&limit, "limit", 50, "page size")
	return c
}

func newBookCmd() *cobra.Command {
	var limit int32
	c := &cobra.Command{
		Use: "book <market>", Short: "Aggregated order book (GET /v1/markets/{symbol}/depth)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetDepthWithResponse(cmd.Context(), args[0], &apiclient.GetDepthParams{Limit: &limit})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			d := resp.JSON200
			n := max(len(d.Bids), len(d.Asks))
			rows := make([][]string, 0, n)
			level := func(ls []apiclient.Level, i int) (string, string, string) {
				if i >= len(ls) {
					return "", "", ""
				}
				return ls[i].Price.String(), ls[i].Qty.String(), strconv.Itoa(int(ls[i].Orders))
			}
			for i := 0; i < n; i++ {
				bp, bq, bn := level(d.Bids, i)
				ap, aq, an := level(d.Asks, i)
				rows = append(rows, []string{bn, bq, bp, "|", ap, aq, an})
			}
			rows = append(rows, []string{"", "", "", "", "", "", ""}, []string{"seq", strconv.FormatInt(d.LastSeq, 10), "", "", "", "", ""})
			return printTable(cmd.OutOrStdout(), []string{"BID_ORDERS", "BID_QTY", "BID_PRICE", "", "ASK_PRICE", "ASK_QTY", "ASK_ORDERS"}, rows)
		},
	}
	c.Flags().Int32Var(&limit, "limit", 10, "levels per side")
	return c
}

func newTradesCmd() *cobra.Command {
	var limit int32
	c := &cobra.Command{
		Use: "trades <market>", Short: "Recent public trades (GET /v1/markets/{symbol}/trades)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListTradesWithResponse(cmd.Context(), args[0], &apiclient.ListTradesParams{Limit: &limit})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Trades))
			for _, t := range resp.JSON200.Trades {
				rows = append(rows, []string{t.TradeID, t.Price.String(), t.Qty.String(), t.QuoteQty.String(), string(t.TakerSide), strconv.FormatInt(t.Seq, 10), t.ExecutedAt.UTC().Format("2006-01-02T15:04:05.000Z")})
			}
			return printTable(cmd.OutOrStdout(), []string{"TRADE_ID", "PRICE", "QTY", "QUOTE_QTY", "TAKER_SIDE", "SEQ", "EXECUTED_AT"}, rows)
		},
	}
	c.Flags().Int32Var(&limit, "limit", 50, "page size")
	return c
}
