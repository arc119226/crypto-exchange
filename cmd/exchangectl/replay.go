package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/matching"
)

// newReplayCmd drives the pure order book (internal/matching) from a JSON-lines
// script without any database or network: the developer loop of Phase 1 and
// the tool behind the golden fixtures in test/fixtures/matching.
func newReplayCmd() *cobra.Command {
	var (
		file      string
		eventsOut string
		depth     int
		snapshot  bool
	)
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay a matching script through the pure order book and print the events",
		Long: `Replay reads a JSON-lines script (first line {"market": {...}}, then one
command per line) and applies it to an empty in-memory order book exactly as the
engine would. It prints every event and the resulting depth (or full snapshot).
The book is deterministic: the same script always prints the same output.

Example scripts live in test/fixtures/matching/*.jsonl.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			in := cmd.InOrStdin()
			if file != "-" {
				f, err := os.Open(file) //nolint:gosec // the path is the operator's CLI argument
				if err != nil {
					return fmt.Errorf("open script: %w", err)
				}
				defer func() { _ = f.Close() }()
				in = f
			}
			script, err := matching.ReadScript(in)
			if err != nil {
				return err
			}
			perCommand, book, runErr := matching.Run(script)
			var sink bytes.Buffer
			out := cmd.OutOrStdout()
			for i, evs := range perCommand {
				if err := printEvents(out, &sink, format, script.Commands[i], evs); err != nil {
					return err
				}
			}
			if eventsOut != "" {
				if err := os.WriteFile(eventsOut, sink.Bytes(), 0o600); err != nil {
					return fmt.Errorf("write events file: %w", err)
				}
			}
			if runErr != nil {
				return runErr
			}
			if snapshot {
				return printSnapshot(out, format, book.Snapshot())
			}
			return printDepth(out, format, book.Depth(depth))
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "script path (JSON lines); '-' for stdin (required)")
	cmd.Flags().StringVar(&eventsOut, "events", "", "also write the events as JSON lines to this file")
	cmd.Flags().IntVar(&depth, "depth", 10, "number of price levels per side to print (0 = all)")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false, "print the full snapshot (every resting order) instead of the depth")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func printEvents(out io.Writer, sink *bytes.Buffer, format string, cmd matching.Command, evs []matching.Event) error {
	for _, e := range evs {
		raw, err := matching.EncodeEvent(e)
		if err != nil {
			return err
		}
		sink.Write(raw)
		sink.WriteByte('\n')
		if format == "json" {
			if _, err := fmt.Fprintf(out, "%s\n", raw); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%-5d %-16s %s\n", cmd.Seq, e.Kind(), describe(e)); err != nil {
			return err
		}
	}
	return nil
}

// describe renders one event as a short human-readable line.
func describe(e matching.Event) string {
	switch ev := e.(type) {
	case matching.Accepted:
		s := fmt.Sprintf("%s %s %s %s", ev.OrderID, ev.AccountID, ev.Side, ev.Type)
		if ev.Type == matching.Market && ev.Side == matching.Buy {
			return s + " quote=" + ev.QuoteQty.String()
		}
		if ev.Type == matching.Market {
			return s + " qty=" + ev.Qty.String()
		}
		return fmt.Sprintf("%s %s price=%s qty=%s", s, ev.TimeInForce, ev.Price, ev.Qty)
	case matching.Trade:
		return fmt.Sprintf("#%d %s x %s = %s  taker=%s(%s %s) maker=%s(%s) maker_remaining=%s",
			ev.Index, ev.Qty, ev.Price, ev.QuoteQty, ev.TakerOrderID, ev.TakerAccountID, ev.TakerSide, ev.MakerOrderID, ev.MakerAccountID, ev.MakerRemaining)
	case matching.Updated:
		return fmt.Sprintf("%s filled=%s (%s) remaining=%s", ev.OrderID, ev.FilledQty, ev.FilledQuote, ev.RemainingQty)
	case matching.Filled:
		return fmt.Sprintf("%s filled=%s (%s)", ev.OrderID, ev.FilledQty, ev.FilledQuote)
	case matching.Cancelled:
		rem := "remaining=" + ev.RemainingQty.String()
		if ev.RemainingQuote.IsPositive() {
			rem = "remaining_quote=" + ev.RemainingQuote.String()
		}
		return fmt.Sprintf("%s reason=%s filled=%s (%s) %s", ev.OrderID, ev.Reason, ev.FilledQty, ev.FilledQuote, rem)
	case matching.Rejected:
		return fmt.Sprintf("%s reason=%s", ev.OrderID, ev.Reason)
	case matching.CancelRejected:
		return fmt.Sprintf("%s reason=%s", ev.OrderID, ev.Reason)
	}
	return fmt.Sprintf("%v", e)
}

func printDepth(out io.Writer, format string, d matching.Depth) error {
	if format == "json" {
		return printJSON(out, d)
	}
	if _, err := fmt.Fprintf(out, "\n%s depth after seq %d\n", d.Symbol, d.LastSeq); err != nil {
		return err
	}
	rows := make([][]string, 0, len(d.Bids)+len(d.Asks))
	for i := len(d.Asks) - 1; i >= 0; i-- {
		rows = append(rows, []string{"ask", d.Asks[i].Price.String(), d.Asks[i].Qty.String(), fmt.Sprint(d.Asks[i].Orders)})
	}
	for _, l := range d.Bids {
		rows = append(rows, []string{"bid", l.Price.String(), l.Qty.String(), fmt.Sprint(l.Orders)})
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(empty book)")
		return err
	}
	return printTable(out, []string{"SIDE", "PRICE", "QTY", "ORDERS"}, rows)
}

func printSnapshot(out io.Writer, format string, s matching.Snapshot) error {
	if format == "json" {
		return printJSON(out, s)
	}
	if _, err := fmt.Fprintf(out, "\n%s snapshot after seq %d\n", s.Symbol, s.LastSeq); err != nil {
		return err
	}
	rows := make([][]string, 0, len(s.Bids)+len(s.Asks))
	add := func(side string, orders []matching.RestingOrder) {
		for _, r := range orders {
			rows = append(rows, []string{side, r.Price.String(), r.Remaining.String(), r.Qty.String(), string(r.OrderID), string(r.AccountID), fmt.Sprint(r.Seq)})
		}
	}
	// asks worst→best on top, bids best→worst below, like a ladder
	asks := make([]matching.RestingOrder, len(s.Asks))
	for i, r := range s.Asks {
		asks[len(s.Asks)-1-i] = r
	}
	add("ask", asks)
	add("bid", s.Bids)
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(empty book)")
		return err
	}
	return printTable(out, []string{"SIDE", "PRICE", "REMAINING", "QTY", "ORDER", "ACCOUNT", "SEQ"}, rows)
}
