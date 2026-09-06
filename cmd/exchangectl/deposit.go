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
