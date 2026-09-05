package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newHealthcheckCmd() *cobra.Command {
	var url string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "GET a readiness URL and exit 0 on 2xx (container HEALTHCHECK helper)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runtimeErr(app.Healthcheck(cmd.Context(), url, timeout))
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://127.0.0.1:9100/readyz", "URL to probe")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "request timeout")
	return cmd
}
