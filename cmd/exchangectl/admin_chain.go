package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

func newAdminDepositsCmd() *cobra.Command {
	c := &cobra.Command{Use: "deposits", Short: "Deposits the chain role has seen (/admin/v1/deposits)"}
	c.AddCommand(newAdminDepositsListCmd())
	return c
}

func newAdminDepositsListCmd() *cobra.Command {
	var (
		status, asset string
		limit, offset int32
	)
	c := &cobra.Command{
		Use: "list", Short: "List deposits, newest first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListDepositsParams{Limit: &limit, Offset: &offset}
			if status != "" {
				s := adminclient.ListDepositsParamsStatus(status)
				params.Status = &s
			}
			if asset != "" {
				params.Asset = &asset
			}
			resp, err := client.ListDepositsWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Deposits))
			for _, d := range resp.JSON200.Deposits {
				credited := ""
				if d.CreditedAt != nil {
					credited = d.CreditedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				rows = append(rows, []string{
					d.ID, d.AccountID, d.Asset, d.Amount.String(), d.TxHash, strconv.FormatInt(d.BlockNumber, 10),
					strconv.Itoa(int(d.Confirmations)), string(d.Status), credited,
				})
			}
			return printTable(cmd.OutOrStdout(), []string{"ID", "ACCOUNT", "ASSET", "AMOUNT", "TX", "BLOCK", "CONFS", "STATUS", "CREDITED"}, rows)
		},
	}
	c.Flags().StringVar(&status, "status", "", "detected, confirming, credited, orphaned, dropped or reversed")
	c.Flags().StringVar(&asset, "asset", "", "asset symbol")
	c.Flags().Int32Var(&limit, "limit", 100, "page size")
	c.Flags().Int32Var(&offset, "offset", 0, "page offset")
	return c
}

func newAdminHotWalletCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hot-wallet",
		Short: "The hot wallet as the chain role keeps it (GET /admin/v1/hot-wallet)",
		Long: "Address, next nonce and the low-balance alert state, with the ledger's custody_hot\n" +
			"balance per asset. The chain's own figure is what reconciliation compares that to.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetHotWalletWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			hw := resp.JSON200
			low := "above the line"
			if hw.Low && hw.LowAlertedAt != nil {
				low = "LOW since " + hw.LowAlertedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "chain %d  address %s  next nonce %d  balance %s\n", hw.ChainID, hw.Address, hw.NextNonce, low)
			rows := make([][]string, 0, len(hw.Balances))
			for _, b := range hw.Balances {
				rows = append(rows, []string{b.Asset, b.Balance.String()})
			}
			return printTable(out, []string{"ASSET", "CUSTODY_HOT"}, rows)
		},
	}
}

func newAdminBreaksCmd() *cobra.Command {
	var limit, offset int32
	c := &cobra.Command{
		Use:   "breaks",
		Short: "Every reconciliation break on record, both kinds (GET /admin/v1/reconciliation/breaks)",
		Long: "A chain break is one asset of one pass whose on-chain custody did not match the\n" +
			"ledger's. A ledger break is one asset whose debits and credits disagree, found by\n" +
			"the admin role checking the ledger against itself; it closes on its own when the\n" +
			"books agree again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListReconciliationBreaksWithResponse(cmd.Context(), &adminclient.ListReconciliationBreaksParams{Limit: &limit, Offset: &offset})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			out := cmd.OutOrStdout()
			rows := make([][]string, 0, len(resp.JSON200.ChainBreaks))
			for _, b := range resp.JSON200.ChainBreaks {
				rows = append(rows, []string{
					b.DetectedAt.UTC().Format("2006-01-02T15:04:05Z"), b.Asset, strconv.FormatInt(b.BlockHeight, 10),
					b.LedgerTotal.String(), b.ChainTotal.String(), b.Diff.String(), b.ReportID,
				})
			}
			_, _ = fmt.Fprintln(out, "chain breaks (ledger custody vs chain):")
			if err := printTable(out, []string{"DETECTED", "ASSET", "BLOCK", "LEDGER", "CHAIN", "DIFF", "REPORT"}, rows); err != nil {
				return err
			}
			rows = rows[:0]
			for _, b := range resp.JSON200.LedgerBreaks {
				resolved := "open"
				if b.ResolvedAt != nil {
					resolved = b.ResolvedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				rows = append(rows, []string{b.DetectedAt.UTC().Format("2006-01-02T15:04:05Z"), b.Asset, b.Debits.String(), b.Credits.String(), b.Diff.String(), resolved})
			}
			_, _ = fmt.Fprintln(out, "ledger breaks (debits vs credits):")
			return printTable(out, []string{"DETECTED", "ASSET", "DEBITS", "CREDITS", "DIFF", "RESOLVED"}, rows)
		},
	}
	c.Flags().Int32Var(&limit, "limit", 100, "page size, per kind")
	c.Flags().Int32Var(&offset, "offset", 0, "page offset, per kind")
	return c
}
