package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// maxSinkBody bounds what one delivery may cost to receive. The exchange's own
// envelopes are kilobytes; anything past this is not one of ours.
const maxSinkBody = 1 << 20

func newWebhookSinkCmd() *cobra.Command {
	var (
		port      int
		secret    string
		tolerance time.Duration
	)
	c := &cobra.Command{
		Use:   "webhook-sink",
		Short: "Receive webhook deliveries locally and verify their signatures",
		Long: "The customer side of docs/webhooks.md, for trying an integration before writing\n" +
			"one. It verifies with the same code the exchange signs with, so a signature that\n" +
			"passes here passes anywhere.\n\n" +
			"A verified delivery is answered 200 and printed. A delivery whose signature does\n" +
			"not check out is answered 401 and printed as REJECTED -- which is also how to\n" +
			"watch the retry schedule work, since the exchange will keep trying.\n\n" +
			"Runs until interrupted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// exchangectl's root uses Execute(), not ExecuteContext, so no
			// command here has ever needed a signal context. This one does:
			// it is the first that does not return on its own.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runWebhookSink(ctx, cmd, port, secret, tolerance)
		},
	}
	c.Flags().IntVar(&port, "port", 9999, "port to listen on")
	c.Flags().StringVar(&secret, "secret", envOr("EXCHANGE_WEBHOOK_SECRET", ""), "the endpoint's signing secret; env EXCHANGE_WEBHOOK_SECRET")
	c.Flags().DurationVar(&tolerance, "tolerance", 5*time.Minute, "how far the delivery timestamp may be from now")
	_ = c.MarkFlagRequired("secret")
	return c
}

func runWebhookSink(ctx context.Context, cmd *cobra.Command, port int, secret string, tolerance time.Duration) error {
	out := cmd.OutOrStdout()
	var mu sync.Mutex
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(io.LimitReader(req.Body, maxSinkBody))
			if err != nil {
				http.Error(w, "cannot read body", http.StatusBadRequest)
				return
			}
			d := sinkDelivery{
				eventID:   req.Header.Get(webhook.EventIDHeader),
				eventType: req.Header.Get(webhook.EventTypeHeader),
				body:      body,
			}
			d.err = webhook.Verify(secret, req.Header.Get(webhook.SignatureHeader),
				req.Header.Get(webhook.TimestampHeader), body, time.Now(), tolerance)

			// One delivery prints as one block even when several arrive at once.
			mu.Lock()
			printSinkDelivery(out, d)
			mu.Unlock()

			if d.err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	}

	// Everything printed goes through say, so the serving goroutine and this
	// one cannot interleave halfway through a delivery.
	say := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(out, format, args...)
	}
	say("listening on :%d, verifying signatures (Ctrl-C to stop)\n", port)

	errs := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		say("stopped\n")
		return nil
	}
}

type sinkDelivery struct {
	eventID   string
	eventType string
	body      []byte
	err       error
}

func printSinkDelivery(w io.Writer, d sinkDelivery) {
	verdict := "verified"
	if d.err != nil {
		verdict = "REJECTED: " + d.err.Error()
	}
	_, _ = fmt.Fprintf(w, "\n%s  %s  %s  [%s]\n",
		time.Now().UTC().Format("15:04:05"), d.eventType, d.eventID, verdict)
	// Pretty-print when it parses, and show the raw bytes when it does not --
	// a body that is not JSON is itself the finding.
	var pretty json.RawMessage
	if err := json.Unmarshal(d.body, &pretty); err == nil {
		if indented, err := json.MarshalIndent(pretty, "  ", "  "); err == nil {
			_, _ = fmt.Fprintf(w, "  %s\n", indented)
			return
		}
	}
	_, _ = fmt.Fprintf(w, "  %s\n", d.body)
}
