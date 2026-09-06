package main

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func newDepositAddressCmd() *cobra.Command {
	var asset string
	cmd := &cobra.Command{
		Use:   "deposit-address",
		Short: "The account's deposit address for an asset (GET /v1/deposit-address)",
		Long: "One address per account per chain: every asset on that chain shares it,\n" +
			"and repeated calls return the same address.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetDepositAddressWithResponse(cmd.Context(), &apiclient.GetDepositAddressParams{Asset: asset})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printTable(cmd.OutOrStdout(), []string{"ASSET", "CHAIN", "ADDRESS"}, [][]string{{
				resp.JSON200.Asset, strconv.FormatInt(resp.JSON200.ChainID, 10), resp.JSON200.Address,
			}})
		},
	}
	cmd.Flags().StringVar(&asset, "asset", "ETH", "asset symbol")
	return cmd
}

func newDepositsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "deposits", Short: "On-chain deposits"}
	list := &cobra.Command{
		Use:   "list",
		Short: "The account's deposits, newest first (GET /v1/deposits)",
		Long: "Only a deposit with status `credited` has reached the balance.\n" +
			"`detected` and `confirming` are on chain but not yet posted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListDepositsWithResponse(cmd.Context(), &apiclient.ListDepositsParams{})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Deposits))
			for _, d := range resp.JSON200.Deposits {
				rows = append(rows, []string{
					d.Asset, d.Amount.String(), d.Status,
					strconv.FormatInt(int64(d.Confirmations), 10) + "/" + strconv.FormatInt(int64(d.RequiredConfirmations), 10),
					d.TxHash,
				})
			}
			return printTable(cmd.OutOrStdout(), []string{"ASSET", "AMOUNT", "STATUS", "CONF", "TX"}, rows)
		},
	}
	cmd.AddCommand(list)
	return cmd
}
