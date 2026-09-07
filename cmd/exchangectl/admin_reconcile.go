package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func newAdminReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use: "reconcile", Short: "Ledger custody against on-chain balances (/admin/v1/reconciliation)",
		Args: cobra.NoArgs,
		Long: "Shows the latest comparison, per asset, of what the ledger says the\n" +
			"exchange holds on chain against what the chain says.\n\n" +
			"DIFF is what is left after the two corrections that account for the two\n" +
			"sides looking at different moments, and it should be exactly zero. A\n" +
			"positive figure means the chain holds more than the ledger was ever\n" +
			"credited for -- money that arrived by a path the scanner cannot see.\n" +
			"Negative means it holds less, and that is the one that cannot wait.\n\n" +
			"This reads the most recent pass rather than running one: the admin role\n" +
			"has no node. The chain role writes a pass every ETH_RECONCILE_INTERVAL.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetReconciliationWithResponse(cmd.Context())
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
				status := "break"
				if l.Balanced {
					status = "ok"
				}
				rows = append(rows, []string{
					l.Asset, l.LedgerTotal.String(), l.ChainTotal.String(),
					l.Uncredited.String(), l.AboveFrontier.String(), l.InFlight.String(),
					l.Diff.String(), status,
				})
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ASSET", "LEDGER", "CHAIN", "UNCREDITED", "ABOVE", "IN FLIGHT", "DIFF", ""}, rows)
		},
	}
}

func newAdminHouseAdjustCmd() *cobra.Command {
	var (
		code      string
		asset     string
		amount    string
		direction string
		reason    string
		key       string
	)
	c := &cobra.Command{
		Use: "house-adjust", Short: "Record value that entered or left custody outside the ledger",
		Args: cobra.NoArgs,
		Long: "Books a custody account against `external` (docs/plan-v1.0.md §6.1.4 g).\n\n" +
			"This is how money that moved without a transaction the exchange produced\n" +
			"gets recorded: a faucet funding the hot wallet, coins moved in from cold\n" +
			"storage, or a reconciliation break whose cause has been established.\n" +
			"Until it is recorded, `admin reconcile` reports it as a break -- correctly,\n" +
			"because the ledger does not know about it.\n\n" +
			"credit means custody gains, debit means it loses.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			value, err := money.ParseAmount(amount)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			body := adminclient.CreateHouseAdjustmentJSONRequestBody{
				Code:      adminclient.HouseAdjustmentRequestCode(code),
				Asset:     asset,
				Amount:    value,
				Direction: adminclient.HouseAdjustmentRequestDirection(direction),
				Reason:    reason,
			}
			if key != "" {
				body.IdempotencyKey = key
			}
			resp, err := client.CreateHouseAdjustmentWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			entry := resp.JSON201
			if entry == nil {
				entry = resp.JSON200
			}
			if entry == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), entry)
			}
			return printTable(cmd.OutOrStdout(), []string{"ENTRY", "KIND", "REASON"},
				[][]string{{fmt.Sprint(entry.ID), entry.Kind, entry.Reason}})
		},
	}
	c.Flags().StringVar(&code, "code", "custody_hot", "custody_hot or custody_deposit_addresses")
	c.Flags().StringVar(&asset, "asset", "", "asset symbol (required)")
	c.Flags().StringVar(&amount, "amount", "", "decimal amount, always positive (required)")
	c.Flags().StringVar(&direction, "direction", "credit", "credit (custody gains) or debit (custody loses)")
	c.Flags().StringVar(&reason, "reason", "", "why this is being recorded (required)")
	c.Flags().StringVar(&key, "idempotency-key", "", "makes a retry safe")
	_ = c.MarkFlagRequired("asset")
	_ = c.MarkFlagRequired("amount")
	_ = c.MarkFlagRequired("reason")
	return c
}
