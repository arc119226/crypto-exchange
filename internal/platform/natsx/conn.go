// Package natsx wraps the NATS connection used for JetStream fan-out.
package natsx

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Connect dials NATS once (the caller retries on startup) and keeps
// reconnecting forever afterwards.
func Connect(url, name string) (*nats.Conn, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.Timeout(5*time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	return nc, nil
}

// HealthCheck returns a readiness probe for the connection.
func HealthCheck(nc *nats.Conn) func(context.Context) error {
	return func(context.Context) error {
		if st := nc.Status(); st != nats.CONNECTED {
			return fmt.Errorf("nats: status %s", st)
		}
		return nil
	}
}
