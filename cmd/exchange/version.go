package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := map[string]string{"version": version, "commit": commit, "date": date, "go": runtime.Version()}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "exchange %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}
