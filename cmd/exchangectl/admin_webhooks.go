package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
)

func newAdminWebhooksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "webhooks",
		Short: "Outbound webhook endpoints (/admin/v1/webhooks)",
		Long: "Customer systems integrate over REST, WebSocket and these. An endpoint is a\n" +
			"URL, the event types it wants, and a signing secret the exchange proves each\n" +
			"delivery with.\n\n" +
			"There is no delete. An integration that is over is disabled, because the\n" +
			"delivery history has to outlive it -- \"we sent it, here is when and what they\n" +
			"answered\" is the whole point of keeping the records.\n\n" +
			"Delivery is at-least-once: the same event_id can arrive twice, and a replay\n" +
			"makes that happen on purpose. Receivers deduplicate (docs/webhooks.md).",
	}
	c.AddCommand(
		newAdminWebhooksCreateCmd(),
		newAdminWebhooksListCmd(),
		newAdminWebhooksUpdateCmd(),
		newAdminWebhooksStatusCmd("disable", "disabled", "Stop delivering to an endpoint"),
		newAdminWebhooksStatusCmd("enable", "active", "Start delivering to an endpoint again"),
		newAdminWebhooksDeliveriesCmd(),
		newAdminWebhooksReplayCmd(),
	)
	return c
}

func newAdminWebhooksCreateCmd() *cobra.Command {
	var (
		rawURL string
		events []string
		label  string
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Register an endpoint (POST /admin/v1/webhooks)",
		Long: "Prints the signing secret once. It is stored encrypted and no later call can\n" +
			"produce it, so an operator who loses it has to create a new endpoint.",
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
			resp, err := client.CreateWebhookEndpointWithResponse(cmd.Context(), adminclient.CreateWebhookEndpointJSONRequestBody{
				URL: rawURL, Events: events, Label: label,
			})
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON201 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON201)
			}
			e := resp.JSON201
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "id      %s\nurl     %s\nevents  %s\nstatus  %s\n",
				e.ID, e.URL, strings.Join(e.Events, ","), e.Status)
			_, _ = fmt.Fprintf(out, "secret  %s\n\nStore the secret now: it is not shown again.\n", e.Secret)
			return nil
		},
	}
	c.Flags().StringVar(&rawURL, "url", "", "where deliveries are POSTed (http:// or https://)")
	c.Flags().StringSliceVar(&events, "events", nil, "event types to receive, comma separated")
	c.Flags().StringVar(&label, "label", "", "a name for whoever is reading the list later")
	_ = c.MarkFlagRequired("url")
	_ = c.MarkFlagRequired("events")
	return c
}

func newAdminWebhooksListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "Endpoints, disabled ones included", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ListWebhookEndpointsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminWebhookEndpoints(cmd, resp.JSON200.WebhookEndpoints)
		},
	}
}

func newAdminWebhooksUpdateCmd() *cobra.Command {
	var (
		rawURL string
		events []string
		label  string
		reason string
	)
	c := &cobra.Command{
		Use:   "update <id>",
		Short: "Change an endpoint's URL, subscriptions or label",
		Long: "The API replaces the whole configuration, so this reads the endpoint first and\n" +
			"sends back the fields no flag changed. Anyone editing the same endpoint at the\n" +
			"same moment wins or loses on ordering; there is one operator key, so that is\n" +
			"a real but small window.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			list, err := client.ListWebhookEndpointsWithResponse(cmd.Context())
			if err != nil {
				return transportError(base, err)
			}
			if list.JSON200 == nil {
				return adminError(list.HTTPResponse, list.Body)
			}
			var current *adminclient.WebhookEndpoint
			for i := range list.JSON200.WebhookEndpoints {
				if list.JSON200.WebhookEndpoints[i].ID == args[0] {
					current = &list.JSON200.WebhookEndpoints[i]
					break
				}
			}
			if current == nil {
				return fmt.Errorf("no webhook endpoint %s", args[0])
			}
			body := adminclient.UpdateWebhookEndpointJSONRequestBody{
				URL: current.URL, Events: current.Events, Label: current.Label, Reason: reason,
			}
			if cmd.Flags().Changed("url") {
				body.URL = rawURL
			}
			if cmd.Flags().Changed("events") {
				body.Events = events
			}
			if cmd.Flags().Changed("label") {
				body.Label = label
			}
			resp, err := client.UpdateWebhookEndpointWithResponse(cmd.Context(), args[0], body)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			return printAdminWebhookEndpoints(cmd, []adminclient.WebhookEndpoint{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&rawURL, "url", "", "new delivery URL")
	c.Flags().StringSliceVar(&events, "events", nil, "new event type list, comma separated")
	c.Flags().StringVar(&label, "label", "", "new label")
	c.Flags().StringVar(&reason, "reason", "", "why it changes (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}

// newAdminWebhooksStatusCmd builds `disable` and `enable`, which are one
// endpoint with two values.
func newAdminWebhooksStatusCmd(use, status, short string) *cobra.Command {
	long := "Disabling also drops whatever is still queued for the endpoint. Queued work for\n" +
		"an inactive endpoint is not pending but unreachable -- nothing delivers it and\n" +
		"nothing cleans it up -- and keeping it would fire a batch of stale events the\n" +
		"day someone turned the integration back on. The delivery history is untouched,\n" +
		"and the number dropped goes in the audit trail."
	if status == "active" {
		long = "Deliveries resume from here. Anything dropped while it was off is not resent\n" +
			"automatically; replay the ones that matter from `webhooks deliveries`."
	}
	var reason string
	c := &cobra.Command{
		Use: use + " <id>", Short: short, Long: long, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.SetWebhookEndpointStatusWithResponse(cmd.Context(), args[0],
				adminclient.SetWebhookEndpointStatusJSONRequestBody{
					Status: adminclient.WebhookEndpointStatus(status), Reason: reason,
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
			return printAdminWebhookEndpoints(cmd, []adminclient.WebhookEndpoint{*resp.JSON200})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why (recorded in the audit trail)")
	_ = c.MarkFlagRequired("reason")
	return c
}

func newAdminWebhooksDeliveriesCmd() *cobra.Command {
	var limit int32
	c := &cobra.Command{
		Use:   "deliveries <endpoint-id>",
		Short: "Delivery attempts for one endpoint, newest first",
		Long: "One row per attempt, not per event. RUN groups the attempts of one pass through\n" +
			"the retry schedule, so a replay's tries are distinguishable from the original's,\n" +
			"and ATTEMPT is the step within that run.\n\n" +
			"An empty CODE means the endpoint never answered at all -- a timeout, a refused\n" +
			"connection, a name that does not resolve -- which is a different conversation\n" +
			"from one that answered 500.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			params := &adminclient.ListWebhookDeliveriesParams{}
			if cmd.Flags().Changed("limit") {
				params.Limit = &limit
			}
			resp, err := client.ListWebhookDeliveriesWithResponse(cmd.Context(), args[0], params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Deliveries))
			for _, d := range resp.JSON200.Deliveries {
				code := ""
				if d.ResponseStatus != nil {
					code = fmt.Sprint(*d.ResponseStatus)
				}
				rows = append(rows, []string{
					d.ID, d.EventType, d.EventID, shortRun(d.RunID), fmt.Sprint(d.Attempt),
					string(d.Status), code, fmt.Sprintf("%dms", d.DurationMs), d.Error,
					d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				})
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"ID", "EVENT TYPE", "EVENT ID", "RUN", "ATTEMPT", "STATUS", "CODE", "TOOK", "ERROR", "AT"}, rows)
		},
	}
	c.Flags().Int32Var(&limit, "limit", 100, "how many attempts to show")
	return c
}

func newAdminWebhooksReplayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "replay <endpoint-id> <delivery-id>",
		Short: "Send the event behind a delivery again",
		Long: "Queues a new run of the event that delivery belongs to and returns; the worker\n" +
			"sends it on its next tick. The old delivery is not changed -- the table is\n" +
			"append-only -- and the replay's attempts join it under a new run.\n\n" +
			"The customer receives the event a second time, with the same event_id. That is\n" +
			"what a replay is, and docs/webhooks.md tells them to deduplicate on it.\n\n" +
			"Refused if the event is still queued (the schedule is going to send it anyway)\n" +
			"or the endpoint is disabled (nothing would pick the run up).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAdminClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.ReplayWebhookDeliveryWithResponse(cmd.Context(), args[0], args[1])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON202 == nil {
				return adminError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON202)
			}
			r := resp.JSON202
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "queued %s for %s, due %s\n",
				r.EventID, r.EndpointID, r.NextAttemptAt.UTC().Format("2006-01-02T15:04:05Z"))
			return nil
		},
	}
}

func printAdminWebhookEndpoints(cmd *cobra.Command, eps []adminclient.WebhookEndpoint) error {
	rows := make([][]string, 0, len(eps))
	for _, e := range eps {
		rows = append(rows, []string{
			e.ID, e.URL, strings.Join(e.Events, ","), e.Label, string(e.Status),
			e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return printTable(cmd.OutOrStdout(),
		[]string{"ID", "URL", "EVENTS", "LABEL", "STATUS", "CREATED"}, rows)
}

// shortRun keeps the delivery table readable. A run only has to be
// distinguishable from the runs beside it, and the full uuid is in --output json.
func shortRun(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
