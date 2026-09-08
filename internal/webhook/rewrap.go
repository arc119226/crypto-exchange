package webhook

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook/sqlcgen"
)

// RewrapSecrets re-seals every endpoint's signing secret -- and the
// previous secret an endpoint still signs with during its grace period --
// under the current key of keys (`exchange keys rewrap --domain webhook`).
// Rows already under it are left alone; a row neither key opens aborts the
// pass rather than being skipped, since the worker could not sign for that
// endpoint either. Rows are locked for the transaction, so a create or a
// rotate that races the pass waits and seals under the current key itself.
func RewrapSecrets(ctx context.Context, tx pgx.Tx, tenant string, keys secretbox.Keyring) (secretbox.Rewrapped, error) {
	var n secretbox.Rewrapped
	if err := keys.Validate(); err != nil || keys.Empty() {
		return n, fmt.Errorf("webhook: rewrap secrets: a current signing key is required")
	}
	q := sqlcgen.New(tx)
	rows, err := q.ListEndpointSecretsForUpdate(ctx, tenant)
	if err != nil {
		return n, fmt.Errorf("webhook: rewrap secrets: list: %w", err)
	}
	for _, row := range rows {
		n.Scanned++
		sealed, changed, err := keys.Rewrap(row.SecretEnc)
		if err != nil {
			return n, fmt.Errorf("webhook: rewrap endpoint %s: %w", row.ID, err)
		}
		previous := row.PreviousSecretEnc
		if len(previous) > 0 {
			var changedPrevious bool
			if previous, changedPrevious, err = keys.Rewrap(previous); err != nil {
				return n, fmt.Errorf("webhook: rewrap endpoint %s (previous secret): %w", row.ID, err)
			}
			changed = changed || changedPrevious
		}
		if !changed {
			continue
		}
		if err := q.SetEndpointSecretEnc(ctx, sqlcgen.SetEndpointSecretEncParams{ID: row.ID, SecretEnc: sealed, PreviousSecretEnc: previous}); err != nil {
			return n, fmt.Errorf("webhook: rewrap endpoint %s: %w", row.ID, err)
		}
		n.Changed++
	}
	return n, nil
}
