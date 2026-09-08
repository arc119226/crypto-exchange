//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/app"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

func masterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, secretbox.KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// TestRewrapMovesEveryDomainToTheCurrentKey is the master key rotation of
// docs/runbooks/key-rotation.md end to end: rows sealed under the old key
// of each domain open under the new key only after `exchange keys rewrap`,
// the pass is idempotent, it audits, and a keyring that cannot open a row
// writes nothing.
func TestRewrapMovesEveryDomainToTheCurrentKey(t *testing.T) {
	ctx := context.Background()
	lh := setupLedger(t)
	l := ledger.New(lh.all, "default")
	require.NoError(t, l.LoadHouseAccounts(ctx))
	rec := audit.NewRecorder("default")

	// --- rows under the old keys
	oldAPI, oldWebhook, oldTOTP := masterKey(t), masterKey(t), masterKey(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)
	users, err := auth.New(lh.all, auth.Config{Tenant: "default", MasterKey: oldAPI, TOTPKey: oldTOTP, Password: auth.TestPasswordParams}, signer, verifier, l, rec)
	require.NoError(t, err)
	sess, err := users.Register(ctx, "rewrap@example.com", "rewrap-password-1", "127.0.0.1")
	require.NoError(t, err)
	apiKey, apiSecret, err := users.CreateAPIKey(ctx, sess.UserID, "rewrap", []string{"read"}, nil)
	require.NoError(t, err)
	_, _, err = users.BootstrapAdmin(ctx, "rewrap-admin@example.com", "rewrap-admin-password")
	require.NoError(t, err)
	enrol, err := users.EnrollTOTP(ctx, "rewrap-admin@example.com")
	require.NoError(t, err)

	store := webhook.NewStore(lh.all, "default", oldWebhook)
	var endpoint webhook.Endpoint
	var liveSecret, graceSecret string
	tx, err := lh.all.Begin(ctx)
	require.NoError(t, err)
	endpoint, graceSecret, err = store.Create(ctx, tx, "https://example.invalid/hook", []string{"trade.executed"}, "rewrap")
	require.NoError(t, err)
	rotated, err := store.RotateSecret(ctx, tx, endpoint.ID, time.Hour, time.Now().UTC())
	require.NoError(t, err)
	liveSecret = rotated.Secret
	require.NoError(t, tx.Commit(ctx))

	sealedAPIKey := func() []byte {
		var b []byte
		require.NoError(t, lh.all.QueryRow(ctx, `SELECT secret_enc FROM auth.api_keys WHERE id = $1`, apiKey.ID).Scan(&b))
		return b
	}
	sealedTOTP := func() []byte {
		var b []byte
		require.NoError(t, lh.all.QueryRow(ctx, `SELECT totp_secret_enc FROM auth.users WHERE email = 'rewrap-admin@example.com'`).Scan(&b))
		return b
	}
	sealedEndpoint := func() (live, grace []byte) {
		require.NoError(t, lh.all.QueryRow(ctx, `SELECT secret_enc, previous_secret_enc FROM webhook.endpoints WHERE id = $1`, endpoint.ID).Scan(&live, &grace))
		return live, grace
	}
	opens := func(key, sealed []byte) (string, bool) {
		s, err := secretbox.Open(key, sealed)
		return s, err == nil
	}

	newAPI, newWebhook, newTOTP := masterKey(t), masterKey(t), masterKey(t)
	migrate := lh.DSN("ex_migrate")

	t.Run("a keyring without the old key writes nothing", func(t *testing.T) {
		_, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapAPIKeys, Keys: secretbox.Keyring{Current: newAPI}}, io.Discard)
		require.Error(t, err)
		_, ok := opens(oldAPI, sealedAPIKey())
		assert.True(t, ok, "the row is untouched")
		var audits int
		require.NoError(t, lh.all.QueryRow(ctx, `SELECT count(*) FROM audit.audit_events WHERE action = 'secrets.rewrap'`).Scan(&audits))
		assert.Zero(t, audits, "the transaction rolled back, audit row included")
	})

	t.Run("api keys", func(t *testing.T) {
		keys := secretbox.Keyring{Current: newAPI, Previous: oldAPI}
		n, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapAPIKeys, Keys: keys}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, secretbox.Rewrapped{Scanned: 1, Changed: 1}, n)
		s, ok := opens(newAPI, sealedAPIKey())
		require.True(t, ok)
		assert.Equal(t, apiSecret, s)
		_, ok = opens(oldAPI, sealedAPIKey())
		assert.False(t, ok, "the old key no longer opens it")

		again, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapAPIKeys, Keys: keys}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, secretbox.Rewrapped{Scanned: 1, Changed: 0}, again, "idempotent")

		// and the api role, now holding only the new key, still verifies the key
		svc, err := auth.New(lh.all, auth.Config{Tenant: "default", MasterKey: newAPI, Password: auth.TestPasswordParams}, nil, nil, l, rec)
		require.NoError(t, err)
		ts := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
		sig := auth.SignRequest(apiSecret, ts, "GET", "/v1/account", nil)
		p, err := svc.VerifyAPIKeyRequest(ctx, auth.APIKeyRequest{KeyID: apiKey.KeyID, Timestamp: ts, Signature: sig, Method: "GET", RequestURI: "/v1/account"})
		require.NoError(t, err)
		assert.Equal(t, sess.UserID, p.UserID)
	})

	t.Run("webhook endpoints, both secrets", func(t *testing.T) {
		keys := secretbox.Keyring{Current: newWebhook, Previous: oldWebhook}
		n, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapWebhook, Keys: keys}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, secretbox.Rewrapped{Scanned: 1, Changed: 1}, n)
		live, grace := sealedEndpoint()
		s, ok := opens(newWebhook, live)
		require.True(t, ok)
		assert.Equal(t, liveSecret, s)
		s, ok = opens(newWebhook, grace)
		require.True(t, ok, "the grace-period secret is rewrapped too")
		assert.Equal(t, graceSecret, s)
		_, ok = opens(oldWebhook, live)
		assert.False(t, ok)

		again, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapWebhook, Keys: keys}, io.Discard)
		require.NoError(t, err)
		assert.Zero(t, again.Changed)
	})

	t.Run("totp secrets", func(t *testing.T) {
		keys := secretbox.Keyring{Current: newTOTP, Previous: oldTOTP}
		n, err := app.Rewrap(ctx, app.RewrapOptions{DSN: migrate, Tenant: "default", Domain: app.RewrapTOTP, Keys: keys}, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, secretbox.Rewrapped{Scanned: 1, Changed: 1}, n)
		s, ok := opens(newTOTP, sealedTOTP())
		require.True(t, ok)
		assert.Equal(t, enrol.Secret, s)
		// the user who never enrolled has no row to scan
		var withSecret int
		require.NoError(t, lh.all.QueryRow(ctx, `SELECT count(*) FROM auth.users WHERE totp_secret_enc IS NOT NULL`).Scan(&withSecret))
		assert.Equal(t, 1, withSecret)
	})

	t.Run("every pass is audited", func(t *testing.T) {
		rows, err := lh.all.Query(ctx, `SELECT target_id, after->>'rewrapped' FROM audit.audit_events WHERE action = 'secrets.rewrap' AND actor_type = 'system' ORDER BY id`)
		require.NoError(t, err)
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Domain    string
			Rewrapped string
		}])
		require.NoError(t, err)
		assert.Equal(t, []struct {
			Domain    string
			Rewrapped string
		}{{"api-keys", "1"}, {"api-keys", "0"}, {"webhook", "1"}, {"webhook", "0"}, {"totp", "1"}}, got)
	})

	t.Run("the roles read through the rotation with the previous key set", func(t *testing.T) {
		// what a role sees between "new key deployed" and "rewrap run": a
		// row still under the old key opens through the keyring
		underOld, err := secretbox.Seal(oldAPI, "still-old")
		require.NoError(t, err)
		s, err := secretbox.Keyring{Current: newAPI, Previous: oldAPI}.Open(underOld)
		require.NoError(t, err)
		assert.Equal(t, "still-old", s)
		assert.NotEqual(t, hex.EncodeToString(oldAPI), hex.EncodeToString(newAPI))
	})
}
