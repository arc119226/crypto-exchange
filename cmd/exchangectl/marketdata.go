package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func newTickerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ticker <market>",
		Short: "24-hour ticker of a market (GET /v1/markets/{symbol}/ticker)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAnonymousClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.GetTickerWithResponse(cmd.Context(), args[0])
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			tk := resp.JSON200
			opt := func(a *apiclient.Amount) string {
				if a == nil {
					return "-"
				}
				return a.String()
			}
			return printTable(cmd.OutOrStdout(),
				[]string{"MARKET", "LAST", "OPEN", "HIGH", "LOW", "CHANGE", "CHANGE_PCT", "VOLUME", "QUOTE_VOLUME", "TRADES", "AT"},
				[][]string{{tk.Market, opt(tk.LastPrice), opt(tk.Open), opt(tk.High), opt(tk.Low), opt(tk.Change), opt(tk.ChangePct),
					tk.Volume.String(), tk.QuoteVolume.String(), strconv.FormatInt(tk.Trades, 10), tk.At.UTC().Format(time.RFC3339)}})
		},
	}
}

func newKlinesCmd() *cobra.Command {
	var (
		interval string
		limit    int32
		from, to string
	)
	c := &cobra.Command{
		Use:   "klines <market>",
		Short: "Candles of a market (GET /v1/markets/{symbol}/klines)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			client, base, err := newAnonymousClient(cmd)
			if err != nil {
				return err
			}
			params := &apiclient.ListKlinesParams{Interval: apiclient.KlineInterval(interval), Limit: &limit}
			if from != "" {
				t, err := time.Parse(time.RFC3339, from)
				if err != nil {
					return fmt.Errorf("--from: %w", err)
				}
				params.From = &t
			}
			if to != "" {
				t, err := time.Parse(time.RFC3339, to)
				if err != nil {
					return fmt.Errorf("--to: %w", err)
				}
				params.To = &t
			}
			resp, err := client.ListKlinesWithResponse(cmd.Context(), args[0], params)
			if err != nil {
				return transportError(base, err)
			}
			if resp.JSON200 == nil {
				return problemError(resp.HTTPResponse, resp.Body)
			}
			if format == "json" {
				return printJSON(cmd.OutOrStdout(), resp.JSON200)
			}
			rows := make([][]string, 0, len(resp.JSON200.Klines))
			for _, k := range resp.JSON200.Klines {
				rows = append(rows, []string{k.Start.UTC().Format(time.RFC3339), k.Open.String(), k.High.String(), k.Low.String(), k.Close.String(),
					k.Volume.String(), k.QuoteVolume.String(), strconv.FormatInt(k.Trades, 10)})
			}
			return printTable(cmd.OutOrStdout(), []string{"START", "OPEN", "HIGH", "LOW", "CLOSE", "VOLUME", "QUOTE_VOLUME", "TRADES"}, rows)
		},
	}
	c.Flags().StringVar(&interval, "interval", "1m", "candle width: 1m, 5m, 15m, 1h, 1d")
	c.Flags().Int32Var(&limit, "limit", 50, "candles to return (max 1000)")
	c.Flags().StringVar(&from, "from", "", "range start, RFC 3339 (default: to - limit x interval)")
	c.Flags().StringVar(&to, "to", "", "range end, RFC 3339 (default: now)")
	return c
}
