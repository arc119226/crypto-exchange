package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

// dayOrInstant accepts either a whole UTC day ("2026-09-01") or an RFC 3339
// instant. A day is what an operator types; an instant is what a script that
// already has one would pass, and rejecting it would be pointless strictness.
func dayOrInstant(flag, v string, endOfDay bool) (*time.Time, error) {
	if v == "" {
		return nil, nil //nolint:nilnil // absent is a real answer here: the server picks the default
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("--%s %q: want a date (2026-09-01) or an RFC 3339 instant", flag, v)
	}
	if endOfDay {
		// The day named is included, so the exclusive end is the midnight
		// after it. Without this, --to 2026-09-08 would report nothing that
		// happened on the 8th.
		t = t.AddDate(0, 0, 1)
	}
	return &t, nil
}

func newAdminRevenueCmd() *cobra.Command {
	var from, to, asset string
	c := &cobra.Command{
		Use:   "revenue",
		Short: "Fees earned and gas paid, per asset (GET /admin/v1/reports/revenue)",
		Long: "One row per asset: maker and taker fees from the trade rows, withdrawal and\n" +
			"deposit fees from the ledger, the gas the exchange paid the chain, and the net.\n\n" +
			"Nothing is converted between assets. Gas is paid in the chain's native coin\n" +
			"whatever was withdrawn, so an ERC-20 row shows fee revenue with no gas beneath\n" +
			"it and the native row carries the gas for every withdrawal on the chain; NET is\n" +
			"the subtraction within one asset and means nothing across two.\n\n" +
			"A withdrawal is counted in the period it confirmed, not the period it was\n" +
			"requested. WDS counts the withdrawals that paid a fee, so GAS divided by it is\n" +
			"what a withdrawal costs -- which is the number the withdrawal fee has to cover.\n\n" +
			"CSV is on the API and in the back office, not here: pipe --output json instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.GetRevenueReportParams{}
			if params.From, err = dayOrInstant("from", from, false); err != nil {
				return err
			}
			if params.To, err = dayOrInstant("to", to, true); err != nil {
				return err
			}
			if asset != "" {
				params.Asset = &asset
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetRevenueReportWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Lines))
			for _, l := range resp.JSON200.Lines {
				rows = append(rows, []string{
					l.Asset, l.MakerFees.String(), l.TakerFees.String(), l.WithdrawalFees.String(),
					l.DepositFees.String(), l.OtherFees.String(), l.GasExpense.String(), l.Net.String(),
					strconv.FormatInt(l.Trades, 10), strconv.FormatInt(l.Withdrawals, 10),
					strconv.FormatInt(l.Deposits, 10),
				})
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s to %s\n",
				resp.JSON200.From.UTC().Format(time.RFC3339), resp.JSON200.To.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ASSET", "MAKER", "TAKER", "WD_FEE", "DEP_FEE", "OTHER", "GAS", "NET", "TRADES", "WDS", "DEPS"}, rows)
		},
	}
	c.Flags().StringVar(&from, "from", "", "start of the period, inclusive: a UTC date or an RFC 3339 instant (default: 30 days before --to)")
	c.Flags().StringVar(&to, "to", "", "end of the period: a UTC date, whose whole day is included, or an RFC 3339 instant, which is exclusive (default: now)")
	c.Flags().StringVar(&asset, "asset", "", "one asset symbol; omit for every asset with activity")
	return c
}
