package main

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func newAssetsCmd() *cobra.Command {
	c := &cobra.Command{Use: "assets", Short: "Inspect listed assets"}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List assets (GET /v1/assets)",
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
			resp, err := client.ListAssetsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return apiError(resp.HTTPResponse, resp.ApplicationProblemJSON500)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAssets(cmd, resp.JSON200.Assets)
		},
	})
	return c
}

func printAssets(cmd *cobra.Command, assets []apiclient.Asset) error {
	rows := make([][]string, 0, len(assets))
	for _, a := range assets {
		contract := "-"
		if a.ContractAddress != nil {
			contract = *a.ContractAddress
		}
		rows = append(rows, []string{
			a.Symbol, a.Name, strconv.FormatInt(a.ChainID, 10), strconv.FormatBool(a.IsNative), contract,
			strconv.Itoa(int(a.Scale)), strconv.Itoa(int(a.RequiredConfirmations)),
			a.MinDeposit.String(), a.MinWithdrawal.String(),
			enabled(a.DepositEnabled), enabled(a.WithdrawEnabled), string(a.Status),
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"SYMBOL", "NAME", "CHAIN", "NATIVE", "CONTRACT", "SCALE", "CONFIRMATIONS", "MIN_DEPOSIT", "MIN_WITHDRAWAL", "DEPOSIT", "WITHDRAW", "STATUS"},
		rows)
}

func enabled(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
