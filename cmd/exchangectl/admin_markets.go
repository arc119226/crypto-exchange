package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

// newAdminMarketsCmd groups the registry commands the operator needs before
// Phase 5 ships the back office (docs/plan-v1.0.md §12 Phase 3).
func newAdminMarketsCmd() *cobra.Command {
	c := &cobra.Command{Use: "markets", Short: "Inspect markets and change their trading status (/admin/v1/markets)"}
	c.AddCommand(newAdminMarketsListCmd(), newAdminMarketsSetStatusCmd(), newAdminMarketsCreateCmd(), newAdminMarketsSetCmd())
	return c
}

func printAdminMarkets(cmd *cobra.Command, markets []adminclient.Market) error {
	rows := make([][]string, 0, len(markets))
	for _, m := range markets {
		rows = append(rows, []string{
			m.Symbol, m.BaseAsset, m.QuoteAsset, m.PriceTick.String(), m.QtyStep.String(), m.MinNotional.String(),
			fmt.Sprintf("%d/%d", m.MakerBps, m.TakerBps), m.SelfTradePolicy, string(m.Status), strconv.Itoa(int(m.Version)),
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"SYMBOL", "BASE", "QUOTE", "PRICE_TICK", "QTY_STEP", "MIN_NOTIONAL", "MAKER/TAKER_BPS", "STP", "STATUS", "VERSION"}, rows)
}

func newAdminMarketsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List markets (GET /admin/v1/markets)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListMarketsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminMarkets(cmd, resp.JSON200.Markets)
		},
	}
}

func newAdminMarketsSetStatusCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "set-status <symbol> <active|halted|cancel_only|delisted>",
		Short: "Change a market's trading status (PUT /admin/v1/markets/{symbol}/status)",
		Long: "Changes the market's status and publishes market.updated. The engine consumes it and " +
			"reloads its registry cache, so the new status applies to the next order without a restart.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.SetMarketStatusWithResponse(cmd.Context(), args[0], adminclient.SetMarketStatusJSONRequestBody{
				Status: adminclient.MarketStatus(args[1]), Reason: reason,
			})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminMarkets(cmd, []adminclient.Market{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why the status changes (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}
