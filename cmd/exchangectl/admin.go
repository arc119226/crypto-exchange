package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// newAdminCmd groups the operator commands. They talk to the admin role
// (:8082) with the static admin API key (docs/plan-v1.0.md §7.4, Phase 2).
func newAdminCmd() *cobra.Command {
	c := &cobra.Command{Use: "admin", Short: "Operator commands (admin API on :8082)"}
	c.PersistentFlags().String("admin-url", envOr("EXCHANGE_ADMIN_URL", "http://localhost:8082"), "admin API base URL")
	c.PersistentFlags().String("admin-key", envOr("EXCHANGE_ADMIN_API_KEY", ""), "admin API key (X-Admin-Api-Key); env EXCHANGE_ADMIN_API_KEY")
	c.AddCommand(newAdminAccountsCmd(), newAdminBalancesCmd(), newAdminFundCmd(), newAdminAdjustCmd(), newAdminTrialBalanceCmd(), newAdminEntriesCmd(), newAdminAuditCmd(), newAdminMarketsCmd(), newAdminWithdrawalsCmd(), newAdminSweepsCmd(), newAdminReconcileCmd(), newAdminHouseAdjustCmd(), newAdminWebhooksCmd())
	return c
}

func newAdminClient(cmd *cobra.Command) (*adminclient.ClientWithResponses, string, error) {
	base, err := cmd.Flags().GetString("admin-url")
	if err != nil {
		return nil, "", err
	}
	key, err := cmd.Flags().GetString("admin-key")
	if err != nil {
		return nil, "", err
	}
	if key == "" {
		return nil, "", fmt.Errorf("admin API key required: --admin-key or EXCHANGE_ADMIN_API_KEY (see .env ADMIN_API_KEY)")
	}
	c, err := adminclient.NewClientWithResponses(base,
		adminclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		adminclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-Admin-Api-Key", key)
			if req.Header.Get(telemetry.RequestIDHeader) == "" {
				req.Header.Set(telemetry.RequestIDHeader, newRequestID())
			}
			return nil
		}),
	)
	if err != nil {
		return nil, "", fmt.Errorf("invalid --admin-url %q: %w", base, err)
	}
	return c, base, nil
}

// adminError renders a non-2xx admin response: the RFC 7807 body when
// present, otherwise the status.
func adminError(resp *http.Response, body []byte) error {
	var p adminclient.Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Title != "" {
		msg := p.Title
		if p.Detail != "" {
			msg += ": " + p.Detail
		}
		return fmt.Errorf("%s (HTTP %d, correlation_id=%s)", msg, resp.StatusCode, p.CorrelationID)
	}
	return fmt.Errorf("unexpected HTTP %d from %s (correlation_id=%s)", resp.StatusCode, resp.Request.URL, resp.Header.Get(telemetry.RequestIDHeader))
}

func newAdminAccountsCmd() *cobra.Command {
	c := &cobra.Command{Use: "accounts", Short: "List or create ledger accounts"}
	var kind string
	list := &cobra.Command{
		Use: "list", Short: "List accounts (GET /admin/v1/accounts)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListAccountsParams{}
			if kind != "" {
				k := adminclient.AccountKind(kind)
				params.Kind = &k
			}
			resp, err := client.ListAccountsWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Accounts))
			for _, a := range resp.JSON200.Accounts {
				house := "-"
				if a.HouseCode != nil {
					house = string(*a.HouseCode)
				}
				owner := "-"
				if a.OwnerUserID != nil {
					owner = *a.OwnerUserID
				}
				rows = append(rows, []string{a.ID, string(a.Kind), house, owner, string(a.Status), a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")})
			}
			return printTable(cmd.OutOrStdout(), []string{"ID", "KIND", "HOUSE_CODE", "OWNER", "STATUS", "CREATED"}, rows)
		},
	}
	list.Flags().StringVar(&kind, "kind", "", "filter: spot|house")
	var owner string
	create := &cobra.Command{
		Use: "create", Short: "Open a spot account (POST /admin/v1/accounts)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			body := adminclient.CreateAccountJSONRequestBody{}
			if owner != "" {
				body.OwnerUserID = &owner
			}
			resp, err := client.CreateAccountWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON201)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), resp.JSON201.ID)
			return err
		},
	}
	create.Flags().StringVar(&owner, "owner", "", "owner user id (optional until Phase 3)")
	c.AddCommand(list, create)
	return c
}

func newAdminBalancesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "balances <account-id>", Short: "Cached balances of an account (GET /admin/v1/accounts/{id}/balances)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetAccountBalancesWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Balances))
			for _, b := range resp.JSON200.Balances {
				rows = append(rows, []string{b.Asset, b.Available.String(), b.Hold.String(), b.Total.String()})
			}
			return printTable(cmd.OutOrStdout(), []string{"ASSET", "AVAILABLE", "HOLD", "TOTAL"}, rows)
		},
	}
}

type adjustFlags struct {
	account, asset, amount, reason, key string
}

func (f *adjustFlags) bind(c *cobra.Command, defaultReason string) {
	c.Flags().StringVar(&f.account, "account", "", "account id (required)")
	c.Flags().StringVar(&f.asset, "asset", "", "asset symbol, e.g. USDC (required)")
	c.Flags().StringVar(&f.amount, "amount", "", "decimal amount, e.g. 10000 (required)")
	c.Flags().StringVar(&f.reason, "reason", defaultReason, "reason recorded in the audit trail")
	c.Flags().StringVar(&f.key, "key", "", "idempotency key (repeat-safe); default: server generated")
	_ = c.MarkFlagRequired("account")
	_ = c.MarkFlagRequired("asset")
	_ = c.MarkFlagRequired("amount")
}

func runAdjust(cmd *cobra.Command, f adjustFlags, direction adminclient.AdjustmentRequestDirection) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	amount, err := money.ParseAmount(f.amount)
	if err != nil || !amount.IsPositive() {
		return fmt.Errorf("--amount must be a positive decimal, got %q", f.amount)
	}
	if f.reason == "" {
		return fmt.Errorf("--reason is required")
	}
	client, base, err := newAdminClient(cmd)
	if err != nil {
		return err
	}
	resp, err := client.CreateAdjustmentWithResponse(cmd.Context(), adminclient.CreateAdjustmentJSONRequestBody{
		AccountID: f.account, Asset: f.asset, Amount: amount, Direction: direction, Reason: f.reason, IdempotencyKey: f.key,
	})
	if err != nil {
		return transportError(base, err)
	}
	entry := resp.JSON201
	replayed := false
	if entry == nil && resp.JSON200 != nil {
		entry, replayed = resp.JSON200, true
	}
	if entry == nil {
		return adminError(resp.HTTPResponse, resp.Body)
	}
	if format == "json" {
		return printJSON(cmd.OutOrStdout(), entry)
	}
	state := "posted"
	if replayed {
		state = "already posted (replay)"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s entry %d key=%s kind=%s\n", state, entry.ID, entry.IdempotencyKey, entry.Kind); err != nil {
		return err
	}
	return printPostings(cmd, entry.Postings)
}

func printPostings(cmd *cobra.Command, postings []adminclient.Posting) error {
	rows := make([][]string, 0, len(postings))
	for _, p := range postings {
		rows = append(rows, []string{p.AccountID, p.Asset, string(p.Bucket), string(p.Direction), p.Amount.String()})
	}
	return printTable(cmd.OutOrStdout(), []string{"ACCOUNT", "ASSET", "BUCKET", "DIRECTION", "AMOUNT"}, rows)
}

func newAdminFundCmd() *cobra.Command {
	var f adjustFlags
	c := &cobra.Command{
		Use: "fund", Short: "Credit an account from the external account (dev faucet / adjustment in the user's favour)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdjust(cmd, f, adminclient.AdjustmentRequestDirectionCredit)
		},
	}
	f.bind(c, "dev faucet")
	return c
}

func newAdminAdjustCmd() *cobra.Command {
	var f adjustFlags
	var direction string
	c := &cobra.Command{
		Use: "adjust", Short: "Post a manual adjustment (POST /admin/v1/ledger/adjustments)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch direction {
			case "credit", "debit":
			default:
				return fmt.Errorf("--direction must be credit or debit")
			}
			return runAdjust(cmd, f, adminclient.AdjustmentRequestDirection(direction))
		},
	}
	f.bind(c, "")
	c.Flags().StringVar(&direction, "direction", "", "credit (add to available) or debit (remove) (required)")
	_ = c.MarkFlagRequired("direction")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminTrialBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use: "trial-balance", Short: "Trial balance per asset and house account balances", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetTrialBalanceWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			tb := resp.JSON200
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), tb)
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "balanced: %t\n", tb.Balanced); err != nil {
				return err
			}
			rows := make([][]string, 0, len(tb.Lines))
			for _, l := range tb.Lines {
				rows = append(rows, []string{l.Asset, l.Debits.String(), l.Credits.String(), l.Diff.String()})
			}
			if err := printTable(out, []string{"ASSET", "DEBITS", "CREDITS", "DIFF"}, rows); err != nil {
				return err
			}
			if len(tb.House) == 0 {
				return nil
			}
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
			rows = rows[:0]
			for _, hb := range tb.House {
				rows = append(rows, []string{string(hb.Code), string(hb.Type), hb.Asset, hb.Balance.String()})
			}
			return printTable(out, []string{"HOUSE", "TYPE", "ASSET", "BALANCE"}, rows)
		},
	}
}

func newAdminEntriesCmd() *cobra.Command {
	var account, refType, refID string
	var limit int32
	c := &cobra.Command{
		Use: "entries", Short: "Journal entries with postings, newest first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListEntriesParams{Limit: &limit}
			if account != "" {
				params.AccountID = &account
			}
			if refType != "" {
				params.RefType = &refType
			}
			if refID != "" {
				params.RefID = &refID
			}
			resp, err := client.ListEntriesWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0)
			for _, e := range resp.JSON200.Entries {
				for _, p := range e.Postings {
					rows = append(rows, []string{strconv.FormatInt(e.ID, 10), e.Kind, e.RefType + ":" + e.RefID, p.AccountID, p.Asset, string(p.Bucket), string(p.Direction), p.Amount.String()})
				}
			}
			return printTable(cmd.OutOrStdout(), []string{"ENTRY", "KIND", "REF", "ACCOUNT", "ASSET", "BUCKET", "DIR", "AMOUNT"}, rows)
		},
	}
	c.Flags().StringVar(&account, "account", "", "only entries touching this account")
	c.Flags().StringVar(&refType, "ref-type", "", "filter by ref type (order, trade, adjustment, ...)")
	c.Flags().StringVar(&refID, "ref-id", "", "filter by ref id")
	c.Flags().Int32Var(&limit, "limit", 50, "max entries")
	return c
}

func newAdminAuditCmd() *cobra.Command {
	var action string
	var limit int32
	c := &cobra.Command{
		Use: "audit", Short: "Audit trail, newest first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListAuditEventsParams{Limit: &limit}
			if action != "" {
				params.Action = &action
			}
			resp, err := client.ListAuditEventsWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Events))
			for _, e := range resp.JSON200.Events {
				rows = append(rows, []string{strconv.FormatInt(e.ID, 10), e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), string(e.ActorType) + "/" + e.ActorID, e.Action, e.TargetType + ":" + e.TargetID, e.CorrelationID})
			}
			return printTable(cmd.OutOrStdout(), []string{"ID", "AT", "ACTOR", "ACTION", "TARGET", "CORRELATION"}, rows)
		},
	}
	c.Flags().StringVar(&action, "action", "", "filter by action")
	c.Flags().Int32Var(&limit, "limit", 50, "max events")
	return c
}
