package eventbus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Stream names (docs/plan-v1.0.md §7.3).
const (
	StreamTrading  = "EX_TRADING"
	StreamChain    = "EX_CHAIN"
	StreamRegistry = "EX_REGISTRY"
)

// DuplicateWindow is how long JetStream remembers Nats-Msg-Id values; a
// relay restart that republishes rows within it produces no duplicates.
const DuplicateWindow = 2 * time.Minute

// StreamConfigs returns the declarations of every stream. Subjects are
// ex.v1.<domain>.<type>.<tenant>.<scope>; one stream per domain group.
func StreamConfigs() []jetstream.StreamConfig {
	subjects := func(domains ...string) []string {
		out := make([]string, 0, len(domains))
		for _, d := range domains {
			out = append(out, SubjectPrefix+"."+d+".*.*.*")
		}
		return out
	}
	base := func(name string, maxAge time.Duration, subj []string) jetstream.StreamConfig {
		return jetstream.StreamConfig{
			Name: name, Subjects: subj, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage,
			MaxAge: maxAge, Duplicates: DuplicateWindow, Discard: jetstream.DiscardOld,
		}
	}
	return []jetstream.StreamConfig{
		base(StreamTrading, 30*24*time.Hour, subjects("order", "trade", "ledger", "balance")),
		base(StreamChain, 30*24*time.Hour, subjects("deposit", "withdrawal", "sweep", "alert")),
		base(StreamRegistry, 90*24*time.Hour, subjects("market", "asset", "fee_schedule", "registry", "user", "reconciliation")),
	}
}

// EnsureStreams creates or updates every stream (idempotent). The relay
// calls it on start; tests call it against a fresh server.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	for _, cfg := range StreamConfigs() {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("eventbus: ensure stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// StreamFor returns the stream a subject belongs to, or "" for a subject
// outside every declaration.
func StreamFor(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) != 6 || parts[0]+"."+parts[1] != SubjectPrefix {
		return ""
	}
	want := SubjectPrefix + "." + parts[2] + ".*.*.*"
	for _, cfg := range StreamConfigs() {
		for _, s := range cfg.Subjects {
			if s == want {
				return cfg.Name
			}
		}
	}
	return ""
}
