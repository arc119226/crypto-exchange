package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func newWithdrawalsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "withdrawals", Short: "On-chain withdrawals"}
	cmd.AddCommand(newWithdrawalsListCmd(), newWithdrawalsCreateCmd())
	return cmd
}

func newWithdrawalsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "The account's withdrawals, newest first (GET /v1/withdrawals)",
		Long: "`pending_review` is waiting for an administrator; `funds_locked` means\n" +
			"the amount is held and the withdrawal is queued for signing.",
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
			resp, err := client.ListWithdrawalsWithResponse(cmd.Context(), &apiclient.ListWithdrawalsParams{})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "ASSET", "AMOUNT", "TO", "STATUS", "NOTE"},
				withdrawalRows(resp.JSON200.Withdrawals))
		},
	}
}

func newWithdrawalsCreateCmd() *cobra.Command {
	var asset, amount, to, key string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Request a withdrawal (POST /v1/withdrawals)",
		Long: "Nothing is held and nothing is signed here: the request is recorded and\n" +
			"the chain role decides it against the withdrawal policy next.\n\n" +
			"--idempotency-key is generated when omitted. Pass your own to make a\n" +
			"retry safe: the same key with the same request returns the original\n" +
			"withdrawal, and the same key with a different one is refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			amt, err := money.ParseAmount(amount)
			if err != nil {
				return fmt.Errorf("--amount: %w", err)
			}
			if key == "" {
				if key, err = randomKey(); err != nil {
					return err
				}
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.CreateWithdrawalWithResponse(cmd.Context(),
				&apiclient.CreateWithdrawalParams{IdempotencyKey: key},
				apiclient.CreateWithdrawalJSONRequestBody{Asset: asset, Amount: amt, ToAddress: to})
			if err != nil {
				return transportError(base, err)
			}
			// 200 is a replay of an earlier request with this key, 201 a new
			// withdrawal. Both carry the withdrawal, so both print it.
			w := resp.JSON201
			if w == nil {
				w = resp.JSON200
			}
			if w == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), w)
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "ASSET", "AMOUNT", "TO", "STATUS", "NOTE"},
				withdrawalRows([]apiclient.Withdrawal{*w}))
		},
	}
	cmd.Flags().StringVar(&asset, "asset", "", "asset symbol (required)")
	cmd.Flags().StringVar(&amount, "amount", "", "amount in the asset's own units (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination address (required)")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency-Key; generated when omitted")
	_ = cmd.MarkFlagRequired("asset")
	_ = cmd.MarkFlagRequired("amount")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func withdrawalRows(ws []apiclient.Withdrawal) [][]string {
	rows := make([][]string, 0, len(ws))
	for _, w := range ws {
		note := "-"
		switch {
		case w.FailureReason != nil && *w.FailureReason != "":
			note = *w.FailureReason
		case w.ReviewNote != nil && *w.ReviewNote != "":
			note = *w.ReviewNote
		}
		rows = append(rows, []string{w.ID, w.Asset, w.Amount.String(), w.ToAddress, w.Status, note})
	}
	return rows
}

// randomKey generates an Idempotency-Key for a caller who did not bring one.
// It is random rather than derived from the request so that two deliberate
// identical withdrawals stay two withdrawals.
func randomKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("idempotency key: %w", err)
	}
	return "ctl-" + hex.EncodeToString(b[:]), nil
}
