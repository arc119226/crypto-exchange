package main

import (
	"fmt"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

// newUserCmd groups the session commands of the reference auth
// (docs/plan-v1.0.md §7.4 auth group).
func newUserCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "Register, log in and inspect the caller (POST /v1/auth/*, GET /v1/account)"}
	var email, password string
	register := &cobra.Command{
		Use: "register", Short: "Register a user and open a spot account (POST /v1/auth/register)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, base, err := newAnonymousClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.RegisterWithResponse(cmd.Context(), apiclient.RegisterJSONRequestBody{Email: openapi_types.Email(email), Password: password})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			return printSession(cmd, *resp.JSON201)
		},
	}
	login := &cobra.Command{
		Use: "login", Short: "Log in (POST /v1/auth/login)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, base, err := newAnonymousClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.LoginWithResponse(cmd.Context(), apiclient.LoginJSONRequestBody{Email: openapi_types.Email(email), Password: password})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			return printSession(cmd, *resp.JSON200)
		},
	}
	for _, sub := range []*cobra.Command{register, login} {
		sub.Flags().StringVar(&email, "email", "", "email address")
		sub.Flags().StringVar(&password, "password", "", "password (8..128 characters)")
		_ = sub.MarkFlagRequired("email")
		_ = sub.MarkFlagRequired("password")
	}

	var refresh string
	logout := &cobra.Command{
		Use: "logout", Short: "Revoke a refresh token (POST /v1/auth/logout)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, base, err := newAnonymousClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.LogoutWithResponse(cmd.Context(), apiclient.LogoutJSONRequestBody{RefreshToken: refresh})
			if err != nil {
				return transportError(base, err)
			}
			if resp.StatusCode() != 204 {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "logged out")
			return err
		},
	}
	logout.Flags().StringVar(&refresh, "refresh-token", "", "refresh token to revoke")
	_ = logout.MarkFlagRequired("refresh-token")

	me := &cobra.Command{
		Use: "me", Short: "Show the authenticated user and account (GET /v1/account)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetAccountWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			a := resp.JSON200
			return printTable(cmd.OutOrStdout(), []string{"USER_ID", "ACCOUNT_ID", "EMAIL", "ROLE", "KYC_LEVEL", "STATUS"},
				[][]string{{a.UserID, a.AccountID, a.Email, string(a.Role), fmt.Sprint(a.KycLevel), string(a.Status)}})
		},
	}
	c.AddCommand(register, login, logout, me)
	return c
}

// printSession prints a session: JSON, or a table plus the export line a
// shell user can paste to authenticate the next commands.
func printSession(cmd *cobra.Command, s apiclient.Session) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if format == "json" {
		return printJSON(cmd.OutOrStdout(), s)
	}
	if err := printTable(cmd.OutOrStdout(), []string{"USER_ID", "ACCOUNT_ID", "ROLE", "EXPIRES_IN"},
		[][]string{{s.UserID, s.AccountID, string(s.Role), fmt.Sprintf("%ds", s.ExpiresIn)}}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "\nexport EXCHANGE_TOKEN=%s\nexport EXCHANGE_REFRESH_TOKEN=%s\n", s.AccessToken, s.RefreshToken)
	return err
}

// newAPIKeysCmd manages HMAC API keys (needs a session token).
func newAPIKeysCmd() *cobra.Command {
	c := &cobra.Command{Use: "api-keys", Short: "Create, list and revoke API keys (/v1/api-keys)"}
	var label, scopes, ips string
	create := &cobra.Command{
		Use: "create", Short: "Create an API key; the secret is printed once (POST /v1/api-keys)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			body := apiclient.CreateAPIKeyJSONRequestBody{}
			if label != "" {
				body.Label = &label
			}
			for _, s := range strings.Split(scopes, ",") {
				if s = strings.TrimSpace(s); s != "" {
					body.Scopes = append(body.Scopes, apiclient.Scope(s))
				}
			}
			if ips != "" {
				list := strings.Split(ips, ",")
				body.IPAllowlist = &list
			}
			resp, err := client.CreateAPIKeyWithResponse(cmd.Context(), body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON201)
			}
			k := resp.JSON201
			if err := printTable(cmd.OutOrStdout(), []string{"ID", "KEY_ID", "LABEL", "SCOPES"}, [][]string{{k.ID, k.KeyID, k.Label, joinScopes(k.Scopes)}}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "\nexport EXCHANGE_API_KEY=%s\nexport EXCHANGE_API_SECRET=%s   # shown once\n", k.KeyID, k.Secret)
			return err
		},
	}
	create.Flags().StringVar(&label, "label", "", "free-text label")
	create.Flags().StringVar(&scopes, "scopes", "read,trade", "comma-separated scopes: read,trade,withdraw")
	create.Flags().StringVar(&ips, "ip-allowlist", "", "comma-separated IPs or CIDRs (empty = any)")

	list := &cobra.Command{
		Use: "list", Short: "List API keys (GET /v1/api-keys)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListAPIKeysWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.APIKeys))
			for _, k := range resp.JSON200.APIKeys {
				revoked := "-"
				if k.RevokedAt != nil {
					revoked = k.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				rows = append(rows, []string{k.ID, k.KeyID, k.Label, joinScopes(k.Scopes), strings.Join(k.IPAllowlist, ","), revoked})
			}
			return printTable(cmd.OutOrStdout(), []string{"ID", "KEY_ID", "LABEL", "SCOPES", "IP_ALLOWLIST", "REVOKED"}, rows)
		},
	}
	revoke := &cobra.Command{
		Use: "revoke <id>", Short: "Revoke an API key (DELETE /v1/api-keys/{id})", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, base, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.RevokeAPIKeyWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.StatusCode() != 204 {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "revoked", args[0])
			return err
		},
	}
	c.AddCommand(create, list, revoke)
	return c
}

func joinScopes(scopes []apiclient.Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}
