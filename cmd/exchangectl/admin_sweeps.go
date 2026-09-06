package main

import (
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

func newAdminSweepsCmd() *cobra.Command {
	c := &cobra.Command{Use: "sweeps", Short: "Collections into the hot wallet (/admin/v1/sweeps)"}
	c.AddCommand(newAdminSweepsListCmd())
	return c
}

func newAdminSweepsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "Recent sweeps, newest first", Args: cobra.NoArgs,
		Long: "A sweep moves a deposit from the address it landed on to the hot wallet\n" +
			"withdrawals are paid from. No user balance is involved: it moves the\n" +
			"exchange's own custody between two of its own house accounts.\n\n" +
			"A native sweep is one transaction. A token sweep is two, because an\n" +
			"address that has only ever received tokens holds no ether and cannot pay\n" +
			"for its own transfer, so the hot wallet funds it first.\n\n" +
			"There is nothing to approve here. Repeated failures are the thing worth\n" +
			"looking at: deposits piling up on addresses the hot wallet cannot spend\n" +
			"from show up first as a withdrawal that cannot be paid.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListSweepsWithResponse(cmd.Context(), &adminclient.ListSweepsParams{})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Sweeps))
			for _, s := range resp.JSON200.Sweeps {
				gas := ""
				if s.GasCost != nil {
					gas = s.GasCost.String()
				}
				status := s.Status
				if s.FailureReason != nil {
					status += " (" + *s.FailureReason + ")"
				}
				rows = append(rows, []string{
					s.ID, s.FromAddress, s.Asset, s.Amount.String(), gas, status,
					s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				})
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "FROM", "ASSET", "AMOUNT", "GAS", "STATUS", "CREATED"}, rows)
		},
	}
}
