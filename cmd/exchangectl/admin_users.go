package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

func newAdminUsersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "users",
		Short: "The user directory: KYC levels and freezes (/admin/v1/users)",
		Long: "Frozen means the user cannot log in, refresh a token or use an API key, and\n" +
			"their spot account takes no orders and no withdrawals -- all in one\n" +
			"transaction. An access token already issued lasts its fifteen minutes.\n" +
			"The last active administrator cannot be frozen.",
	}
	c.AddCommand(newAdminUsersListCmd(), newAdminUsersGetCmd(), newAdminUsersSetKYCCmd(),
		newAdminUsersStatusCmd("freeze", "frozen", "Freeze a user everywhere"),
		newAdminUsersStatusCmd("unfreeze", "active", "Release a frozen user"))
	return c
}

func printAdminUsers(cmd *cobra.Command, users []adminclient.User) error {
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		totp := ""
		if u.TotpEnabled {
			totp = "yes"
		}
		rows = append(rows, []string{
			u.ID, u.Email, string(u.Role), strconv.Itoa(u.KycLevel), string(u.Status), totp,
			u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return printTable(cmd.OutOrStdout(), []string{"ID", "EMAIL", "ROLE", "KYC", "STATUS", "TOTP", "CREATED"}, rows)
}

func newAdminUsersListCmd() *cobra.Command {
	var (
		email, status, role string
		limit, offset       int32
	)
	c := &cobra.Command{
		Use: "list", Short: "List users, newest first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListUsersParams{Limit: &limit, Offset: &offset}
			if email != "" {
				params.Email = &email
			}
			if status != "" {
				s := adminclient.UserStatus(status)
				params.Status = &s
			}
			if role != "" {
				r := adminclient.UserRole(role)
				params.Role = &r
			}
			resp, err := client.ListUsersWithResponse(cmd.Context(), params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminUsers(cmd, resp.JSON200.Users)
		},
	}
	c.Flags().StringVar(&email, "email", "", "fragment matched anywhere in the address (no wildcards)")
	c.Flags().StringVar(&status, "status", "", "active or frozen")
	c.Flags().StringVar(&role, "role", "", "user or admin")
	c.Flags().Int32Var(&limit, "limit", 100, "page size")
	c.Flags().Int32Var(&offset, "offset", 0, "page offset")
	return c
}

func newAdminUsersGetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "get <user-id>", Short: "One user", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetUserWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminUsers(cmd, []adminclient.User{*resp.JSON200})
		},
	}
}

func newAdminUsersSetKYCCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "set-kyc <user-id> <level>",
		Short: "Move a user between KYC levels (PUT /admin/v1/users/{id}/kyc-level)",
		Long: "The withdrawal policy reads the level on each request, so the next withdrawal is\n" +
			"decided under the new level's limits; nothing already pending is re-decided.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			level, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("level must be a number: %w", err)
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.SetUserKycLevelWithResponse(cmd.Context(), args[0], adminclient.SetUserKycLevelJSONRequestBody{KycLevel: level, Reason: reason})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminUsers(cmd, []adminclient.User{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminUsersStatusCmd(use, status, short string) *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use: use + " <user-id>", Short: short + " (PUT /admin/v1/users/{id}/status)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.SetUserStatusWithResponse(cmd.Context(), args[0], adminclient.SetUserStatusJSONRequestBody{
				Status: adminclient.UserStatus(status), Reason: reason,
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
			return printAdminUsers(cmd, []adminclient.User{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}
