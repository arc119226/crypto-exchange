package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func newMarketsCmd() *cobra.Command {
	c := &cobra.Command{Use: "markets", Short: "Inspect listed markets"}
	c.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List markets (GET /v1/markets)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				format, err := outputFormat(cmd)
				if err != nil {
					return err
				}
				client, base, err := newClient(cmd)
				if err != nil {
					return err
				}
				resp, err := client.ListMarketsWithResponse(cmd.Context())
				if err != nil {
					return transportError(base, err)
				}
				if resp.JSON200 == nil {
					return apiError(resp.HTTPResponse, resp.ApplicationProblemJSON500)
				}
				if format == "json" {
					return printJSON(cmd.OutOrStdout(), resp.JSON200)
				}
				return printMarkets(cmd, resp.JSON200.Markets)
			},
		},
		&cobra.Command{
			Use:   "get <symbol>",
			Short: "Show one market (GET /v1/markets/{symbol})",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				format, err := outputFormat(cmd)
				if err != nil {
					return err
				}
				client, base, err := newClient(cmd)
				if err != nil {
					return err
				}
				resp, err := client.GetMarketWithResponse(cmd.Context(), args[0])
				if err != nil {
					return transportError(base, err)
				}
				if resp.JSON200 == nil {
					problem := resp.ApplicationProblemJSON404
					if problem == nil {
						problem = resp.ApplicationProblemJSON500
					}
					return apiError(resp.HTTPResponse, problem)
				}
				if format == "json" {
					return printJSON(cmd.OutOrStdout(), resp.JSON200)
				}
				return printMarkets(cmd, []apiclient.Market{*resp.JSON200})
			},
		},
	)
	return c
}

func printMarkets(cmd *cobra.Command, markets []apiclient.Market) error {
	rows := make([][]string, 0, len(markets))
	for _, m := range markets {
		maxQty := "-"
		if m.MaxQty != nil {
			maxQty = m.MaxQty.String()
		}
		slippage := "-"
		if m.MaxSlippageBps != nil {
			slippage = strconv.Itoa(int(*m.MaxSlippageBps))
		}
		rows = append(rows, []string{
			m.Symbol, m.BaseAsset, m.QuoteAsset,
			m.PriceTick.String(), m.QtyStep.String(), m.MinNotional.String(), maxQty, slippage,
			fmt.Sprintf("%d/%d", m.MakerBps, m.TakerBps), string(m.SelfTradePolicy), string(m.Status),
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"SYMBOL", "BASE", "QUOTE", "PRICE_TICK", "QTY_STEP", "MIN_NOTIONAL", "MAX_QTY", "MAX_SLIPPAGE_BPS", "MAKER/TAKER_BPS", "STP", "STATUS"},
		rows)
}
