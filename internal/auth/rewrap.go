package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/auth/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
)

// RewrapAPIKeys re-seals every API-key secret of the tenant under the
// current key of keys (`exchange keys rewrap --domain api-keys`). Rows
// already under it are left alone, so the pass is idempotent; a row that
// neither key opens aborts the pass, because a key that cannot be verified
// is an outage nobody has noticed yet, not a row to skip. Rows are locked
// for the transaction, so a key created meanwhile waits and is sealed under
// the current key by the caller that creates it.
func RewrapAPIKeys(ctx context.Context, tx pgx.Tx, tenant string, keys secretbox.Keyring) (secretbox.Rewrapped, error) {
	var n secretbox.Rewrapped
	if err := keys.Validate(); err != nil || keys.Empty() {
		return n, fmt.Errorf("auth: rewrap api keys: a current master key is required")
	}
	q := sqlcgen.New(tx)
	rows, err := q.ListAPIKeySecretsForUpdate(ctx, tenant)
	if err != nil {
		return n, fmt.Errorf("auth: rewrap api keys: list: %w", err)
	}
	for _, row := range rows {
		n.Scanned++
		sealed, changed, err := keys.Rewrap(row.SecretEnc)
		if err != nil {
			return n, fmt.Errorf("auth: rewrap api key %s: %w", row.ID, err)
		}
		if !changed {
			continue
		}
		if err := q.SetAPIKeySecretEnc(ctx, sqlcgen.SetAPIKeySecretEncParams{ID: row.ID, SecretEnc: sealed}); err != nil {
			return n, fmt.Errorf("auth: rewrap api key %s: %w", row.ID, err)
		}
		n.Changed++
	}
	return n, nil
}

// RewrapTOTPSecrets is RewrapAPIKeys for administrators' TOTP secrets
// (`exchange keys rewrap --domain totp`).
func RewrapTOTPSecrets(ctx context.Context, tx pgx.Tx, tenant string, keys secretbox.Keyring) (secretbox.Rewrapped, error) {
	var n secretbox.Rewrapped
	if err := keys.Validate(); err != nil || keys.Empty() {
		return n, fmt.Errorf("auth: rewrap totp secrets: a current totp key is required")
	}
	q := sqlcgen.New(tx)
	rows, err := q.ListTOTPSecretsForUpdate(ctx, tenant)
	if err != nil {
		return n, fmt.Errorf("auth: rewrap totp secrets: list: %w", err)
	}
	for _, row := range rows {
		n.Scanned++
		sealed, changed, err := keys.Rewrap(row.TotpSecretEnc)
		if err != nil {
			return n, fmt.Errorf("auth: rewrap totp secret of user %s: %w", row.ID, err)
		}
		if !changed {
			continue
		}
		if err := q.SetTOTPSecretEnc(ctx, sqlcgen.SetTOTPSecretEncParams{ID: row.ID, TotpSecretEnc: sealed}); err != nil {
			return n, fmt.Errorf("auth: rewrap totp secret of user %s: %w", row.ID, err)
		}
		n.Changed++
	}
	return n, nil
}
