package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// The rest of the registry from the terminal. An asset or a market has too
// many fields for flags, so create and set read the request body from a
// JSON file (--file, or - for stdin) and take the reason as a flag; the
// small ones -- fee schedules, withdrawal limits -- are flags.

// readBodyFile decodes a JSON request body from a file or stdin.
func readBodyFile(path string, v any) error {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path) //nolint:gosec // an operator's own file, named on the command line
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func newAdminAssetsCmd() *cobra.Command {
	c := &cobra.Command{Use: "assets", Short: "Assets of the tenant (/admin/v1/assets)"}
	c.AddCommand(newAdminAssetsListCmd(), newAdminAssetsGetCmd(), newAdminAssetsCreateCmd(), newAdminAssetsSetCmd())
	return c
}

func printAdminAssets(cmd *cobra.Command, assets []adminclient.Asset) error {
	rows := make([][]string, 0, len(assets))
	for _, a := range assets {
		contract := "native"
		if a.ContractAddress != nil {
			contract = *a.ContractAddress
		}
		rows = append(rows, []string{
			a.Symbol, a.Name, strconv.FormatInt(a.ChainID, 10), contract, fmt.Sprintf("%d/%d", a.Scale, a.DisplayScale),
			strconv.Itoa(int(a.RequiredConfirmations)), a.MinWithdrawal.String(), a.WithdrawalFee.String(), a.SweepThreshold.String(),
			onOff(a.DepositEnabled), onOff(a.WithdrawEnabled), string(a.Status), strconv.Itoa(int(a.Version)),
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"SYMBOL", "NAME", "CHAIN", "CONTRACT", "SCALE", "CONFS", "MIN_WD", "WD_FEE", "SWEEP_AT", "DEPOSITS", "WITHDRAWALS", "STATUS", "V"}, rows)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func newAdminAssetsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List assets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListAssetsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminAssets(cmd, resp.JSON200.Assets)
		},
	}
}

func newAdminAssetsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "get <symbol>", Short: "One asset", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetAssetWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminAssets(cmd, []adminclient.Asset{*resp.JSON200})
		},
	}
}

func newAdminAssetsCreateCmd() *cobra.Command {
	var file, reason string
	c := &cobra.Command{
		Use:   "create --file <asset.json>",
		Short: "List a new asset (POST /admin/v1/assets)",
		Long: "The file is the request body: symbol, name, chain_id, contract_address (omit for the\n" +
			"native coin), is_native, scale, display_scale, required_confirmations, min_deposit,\n" +
			"min_withdrawal, withdrawal_fee, sweep_threshold, deposit_enabled, withdraw_enabled,\n" +
			"status. Amounts are decimal strings. `exchangectl admin assets get ETH -o json` shows\n" +
			"the shape. Publishes asset.updated; the engine reloads.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			var body adminclient.CreateAssetJSONRequestBody
			if err := readBodyFile(file, &body); err != nil {
				return err
			}
			body.Reason = reason
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.CreateAssetWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON201)
			}
			return printAdminAssets(cmd, []adminclient.Asset{*resp.JSON201})
		},
	}
	c.Flags().StringVar(&file, "file", "", "JSON request body (- for stdin)")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("file")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminAssetsSetCmd() *cobra.Command {
	var file, reason string
	c := &cobra.Command{
		Use:   "set <symbol> --file <asset.json>",
		Short: "Replace an asset's writable fields (PUT /admin/v1/assets/{symbol})",
		Long: "Every writable field, from the file; a body equal to the current row changes\n" +
			"nothing and emits no event. Start from `assets get <symbol> -o json`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			var body adminclient.UpdateAssetJSONRequestBody
			if err := readBodyFile(file, &body); err != nil {
				return err
			}
			body.Reason = reason
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.UpdateAssetWithResponse(cmd.Context(), args[0], body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminAssets(cmd, []adminclient.Asset{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&file, "file", "", "JSON request body (- for stdin)")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("file")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminMarketsCreateCmd() *cobra.Command {
	var file, reason string
	c := &cobra.Command{
		Use:   "create --file <market.json>",
		Short: "List a new market (POST /admin/v1/markets)",
		Long: "The file is the request body: symbol, base_asset, quote_asset, price_tick, qty_step,\n" +
			"min_notional, max_qty (optional), max_slippage_bps (optional), fee_schedule,\n" +
			"self_trade_policy, status. Assets and the schedule must exist. Publishes\n" +
			"market.updated; the engine opens the book on reload.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			var body adminclient.CreateMarketJSONRequestBody
			if err := readBodyFile(file, &body); err != nil {
				return err
			}
			body.Reason = reason
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.CreateMarketWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON201)
			}
			return printAdminMarkets(cmd, []adminclient.Market{*resp.JSON201})
		},
	}
	c.Flags().StringVar(&file, "file", "", "JSON request body (- for stdin)")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("file")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminMarketsSetCmd() *cobra.Command {
	var file, reason string
	c := &cobra.Command{
		Use:   "set <symbol> --file <market.json>",
		Short: "Replace a market's writable fields (PUT /admin/v1/markets/{symbol})",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			var body adminclient.UpdateMarketJSONRequestBody
			if err := readBodyFile(file, &body); err != nil {
				return err
			}
			body.Reason = reason
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.UpdateMarketWithResponse(cmd.Context(), args[0], body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminMarkets(cmd, []adminclient.Market{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&file, "file", "", "JSON request body (- for stdin)")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("file")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminFeeSchedulesCmd() *cobra.Command {
	c := &cobra.Command{Use: "fee-schedules", Short: "Maker/taker fees in basis points (/admin/v1/fee-schedules)"}
	c.AddCommand(newAdminFeeSchedulesListCmd(), newAdminFeeSchedulesSetCmd("create", true), newAdminFeeSchedulesSetCmd("set", false))
	return c
}

func printAdminFeeSchedules(cmd *cobra.Command, fees []adminclient.FeeSchedule) error {
	rows := make([][]string, 0, len(fees))
	for _, f := range fees {
		rows = append(rows, []string{f.Name, strconv.Itoa(int(f.MakerBps)), strconv.Itoa(int(f.TakerBps)), f.EffectiveFrom.UTC().Format("2006-01-02T15:04:05Z"), strconv.Itoa(int(f.Version))})
	}
	return printTable(cmd.OutOrStdout(), []string{"NAME", "MAKER_BPS", "TAKER_BPS", "EFFECTIVE_FROM", "V"}, rows)
}

func newAdminFeeSchedulesListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List fee schedules", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListFeeSchedulesWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminFeeSchedules(cmd, resp.JSON200.FeeSchedules)
		},
	}
}

func newAdminFeeSchedulesSetCmd(use string, create bool) *cobra.Command {
	var (
		maker, taker int32
		reason       string
	)
	short := "Change a schedule's rates (PUT /admin/v1/fee-schedules/{name})"
	if create {
		short = "Add a fee schedule (POST /admin/v1/fee-schedules)"
	}
	c := &cobra.Command{
		Use: use + " <name> --maker-bps N --taker-bps N", Short: short, Args: cobra.ExactArgs(1),
		Long: "Every market on the schedule pays the new rates from the next trade. Publishes\n" +
			"fee_schedule.updated; the engine reloads.",
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			var (
				fee  *adminclient.FeeSchedule
				resp *http.Response
				body []byte
			)
			if create {
				r, err := client.CreateFeeScheduleWithResponse(cmd.Context(), adminclient.CreateFeeScheduleJSONRequestBody{Name: args[0], MakerBps: maker, TakerBps: taker, Reason: reason})
				if err != nil {
					return transportError(base, err)
				}
				fee, resp, body = r.JSON201, r.HTTPResponse, r.Body
			} else {
				r, err := client.UpdateFeeScheduleWithResponse(cmd.Context(), args[0], adminclient.UpdateFeeScheduleJSONRequestBody{MakerBps: maker, TakerBps: taker, Reason: reason})
				if err != nil {
					return transportError(base, err)
				}
				fee, resp, body = r.JSON200, r.HTTPResponse, r.Body
			}
			if fee == nil {
				return adminError(resp, body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), fee)
			}
			return printAdminFeeSchedules(cmd, []adminclient.FeeSchedule{*fee})
		},
	}
	c.Flags().Int32Var(&maker, "maker-bps", 0, "maker fee in basis points")
	c.Flags().Int32Var(&taker, "taker-bps", 0, "taker fee in basis points")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("maker-bps")
	_ = c.MarkFlagRequired("taker-bps")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminWithdrawalLimitsCmd() *cobra.Command {
	c := &cobra.Command{Use: "withdrawal-limits", Short: "Withdrawal limits per asset and KYC level (/admin/v1/withdrawal-limits)"}
	c.AddCommand(newAdminWithdrawalLimitsListCmd(), newAdminWithdrawalLimitsSetCmd())
	return c
}

func printAdminWithdrawalLimits(cmd *cobra.Command, limits []adminclient.WithdrawalLimit) error {
	rows := make([][]string, 0, len(limits))
	for _, l := range limits {
		review := "by amount"
		if l.RequireManualReview {
			review = "always"
		}
		rows = append(rows, []string{l.Asset, strconv.Itoa(l.KycLevel), l.AutoApproveLimit.String(), l.DailyLimit.String(), review, strconv.Itoa(int(l.Version))})
	}
	return printTable(cmd.OutOrStdout(), []string{"ASSET", "KYC", "AUTO_APPROVE", "DAILY", "REVIEW", "V"}, rows)
}

func newAdminWithdrawalLimitsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List withdrawal limits", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListWithdrawalLimitsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminWithdrawalLimits(cmd, resp.JSON200.WithdrawalLimits)
		},
	}
}

func newAdminWithdrawalLimitsSetCmd() *cobra.Command {
	var (
		auto, daily, reason string
		review              bool
	)
	c := &cobra.Command{
		Use:   "set <asset> <kyc-level> --auto-approve A --daily D",
		Short: "Set the limits of one asset at one level (PUT /admin/v1/withdrawal-limits/{asset}/{kyc_level})",
		Long: "Creates the row when there is none. The next withdrawal reads the new numbers.\n" +
			"A single withdrawal up to the auto-approve limit goes out on its own; larger ones\n" +
			"wait for a person; over the daily limit they are refused.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			level, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("kyc level must be a number: %w", err)
			}
			autoAmt, err := money.ParseAmount(auto)
			if err != nil {
				return fmt.Errorf("--auto-approve: %w", err)
			}
			dailyAmt, err := money.ParseAmount(daily)
			if err != nil {
				return fmt.Errorf("--daily: %w", err)
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.SetWithdrawalLimitWithResponse(cmd.Context(), args[0], level, adminclient.SetWithdrawalLimitJSONRequestBody{
				AutoApproveLimit: autoAmt, DailyLimit: dailyAmt, RequireManualReview: review, Reason: reason,
			})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminWithdrawalLimits(cmd, []adminclient.WithdrawalLimit{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&auto, "auto-approve", "", "largest single withdrawal that skips review")
	c.Flags().StringVar(&daily, "daily", "", "rolling 24h cap; over it the request is refused")
	c.Flags().BoolVar(&review, "review-all", false, "send every withdrawal at this level to review")
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("auto-approve")
	_ = c.MarkFlagRequired("daily")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminReloadCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "reload",
		Short: "Ask every engine to reload its registry cache (POST /admin/v1/engine/reload)",
		Long: "Every registry change already tells the engine to reload. This asks for one with\n" +
			"nothing changed: after a seed, or when in doubt. 202: the request is in the outbox.",
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
			resp, err := client.RequestReloadWithResponse(cmd.Context(), adminclient.RequestReloadJSONRequestBody{Reason: reason})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON202 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON202)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reload requested: event %s at %s\n", resp.JSON202.EventID, resp.JSON202.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"))
			return nil
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}
