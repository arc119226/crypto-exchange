package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// outputFormat reads --output and validates it.
func outputFormat(cmd *cobra.Command) (string, error) {
	f, err := cmd.Flags().GetString("output")
	if err != nil {
		return "", err
	}
	switch f {
	case "table", "json":
		return f, nil
	default:
		return "", fmt.Errorf("unknown --output %q (want table or json)", f)
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printTable(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(r, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
