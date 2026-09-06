package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

func newAdminWithdrawalsCmd() *cobra.Command {
	c := &cobra.Command{Use: "withdrawals", Short: "The withdrawal review queue (/admin/v1/withdrawals)"}
	c.AddCommand(newAdminWithdrawalsListCmd(), newAdminWithdrawalsReviewCmd(), newAdminWithdrawalsResolveCmd())
	return c
}

func newAdminWithdrawalsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "Withdrawals awaiting review, oldest first", Args: cobra.NoArgs,
		Long: "A withdrawal is here because it broke a limit, not because it is\n" +
			"illegitimate: the limits decide who approves it, not whether it is allowed.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListWithdrawalsForReviewWithResponse(cmd.Context(), &adminclient.ListWithdrawalsForReviewParams{})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "ACCOUNT", "ASSET", "AMOUNT", "TO", "REQUESTED"},
				adminWithdrawalRows(resp.JSON200.Withdrawals))
		},
	}
}

func newAdminWithdrawalsReviewCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "review <id> <approve|reject>",
		Short: "Approve or reject a withdrawal awaiting review",
		Long: "Approving does not move money: it marks the withdrawal for the chain\n" +
			"worker, which locks the funds and hands it to the signer. A rejection\n" +
			"must carry --note, because the user and the next operator both need to\n" +
			"know why.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			decision := adminclient.WithdrawalReviewRequestDecision(args[1])
			if !decision.Valid() {
				return fmt.Errorf("decision must be approve or reject, got %q", args[1])
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			body := adminclient.ReviewWithdrawalJSONRequestBody{Decision: decision}
			if note != "" {
				body.Note = &note
			}
			resp, err := client.ReviewWithdrawalWithResponse(cmd.Context(), args[0], body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "ACCOUNT", "ASSET", "AMOUNT", "TO", "REQUESTED"},
				adminWithdrawalRows([]adminclient.AdminWithdrawal{*resp.JSON200}))
		},
	}
	c.Flags().StringVar(&note, "note", "", "why; required when rejecting")
	return c
}

func newAdminWithdrawalsResolveCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "resolve <id> <bump|cancel_nonce|refund|retry>",
		Short: "Unstick a withdrawal the machine could not finish",
		Long: "Which action applies depends on where the withdrawal stopped.\n\n" +
			"A broadcast withdrawal has a transaction in flight:\n" +
			"  bump          re-send it on the same nonce with a higher fee\n" +
			"  cancel_nonce  displace it with a self-transfer on that nonce\n\n" +
			"A failed/on_chain withdrawal has a transaction that reverted:\n" +
			"  refund        return the amount to the user and end it\n" +
			"  retry         put it back on hold and send it again\n\n" +
			"Nothing happens in the admin role, which holds no key: the request is\n" +
			"recorded and the chain worker applies it on its next tick. --note is\n" +
			"required, because every one of these is a person overriding the\n" +
			"machine on someone else's money.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			action := adminclient.WithdrawalResolveRequestAction(args[1])
			if !action.Valid() {
				return fmt.Errorf("action must be bump, cancel_nonce, refund or retry, got %q", args[1])
			}
			if note == "" {
				return fmt.Errorf("--note is required: say why")
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ResolveWithdrawalWithResponse(cmd.Context(), args[0],
				adminclient.ResolveWithdrawalJSONRequestBody{Action: action, Note: &note})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "ACCOUNT", "ASSET", "AMOUNT", "TO", "REQUESTED"},
				adminWithdrawalRows([]adminclient.AdminWithdrawal{*resp.JSON200}))
		},
	}
	c.Flags().StringVar(&note, "note", "", "why; required")
	return c
}

func adminWithdrawalRows(ws []adminclient.AdminWithdrawal) [][]string {
	rows := make([][]string, 0, len(ws))
	for _, w := range ws {
		rows = append(rows, []string{
			w.ID, w.AccountID, w.Asset, w.Amount.String(), w.ToAddress,
			w.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return rows
}
