//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every operations page, through a signed-in browser, on the ex_admin pool.
// Mostly reads on a fresh database -- each page has to say "nothing yet"
// rather than fail -- plus the writes that need no chain: a house
// adjustment, and a webhook endpoint with its secret shown once.
func TestAdminUIOperationsPages(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)
	srv := adminUIServer(t, h, loginLimitForTests)
	b := newBrowser(t, srv)
	signIn(t, b, h)

	for path, want := range map[string]string{
		"/admin/ledger":         ">balanced<",
		"/admin/withdrawals":    "Nothing waiting.",
		"/admin/chain":          "No hot wallet recorded",
		"/admin/reconciliation": "No pass has been recorded yet.",
		"/admin/audit":          "auth.admin.login",
		"/admin/webhooks":       "No endpoints.",
	} {
		resp, body := b.get(path)
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Contains(t, body, want, path)
		assert.NotContains(t, body, "Could not read", path)
	}

	t.Run("the ledger page looks up an account", func(t *testing.T) {
		acct, err := h.ledgerHarness.svc.SpotAccountOf(ctx, h.all, h.admin.ID)
		require.NoError(t, err)
		_, body := b.get("/admin/ledger?account_id=" + acct.ID)
		assert.Contains(t, body, "Account <span class=\"mono\">"+acct.ID+"</span>")
		assert.Contains(t, body, "No balances.")
		_, body = b.get("/admin/ledger?account_id=00000000-0000-0000-0000-000000000000")
		assert.Contains(t, body, "No account 00000000-0000-0000-0000-000000000000.")
	})

	t.Run("a house adjustment from the reconciliation page", func(t *testing.T) {
		resp, _ := b.post("/admin/ledger/house-adjustments", url.Values{"code": {"custody_hot"}, "asset": {"ETH"}, "amount": {"0.25"}, "direction": {"debit"}, "reason": {"gas booked by hand"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, "/admin/reconciliation", resp.Header.Get("Location"))
		_, body := b.get("/admin/reconciliation")
		assert.Contains(t, body, "House adjustment posted as entry")
		assert.Contains(t, body, "gas booked by hand")
		_, body = b.get("/admin/ledger")
		assert.Contains(t, body, "gas booked by hand", "and it is an entry like any other")

		resp, _ = b.post("/admin/ledger/house-adjustments", url.Values{"code": {"custody_hot"}, "asset": {"ETH"}, "amount": {"lots"}, "direction": {"debit"}, "reason": {"x"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		_, body = b.get("/admin/reconciliation")
		assert.Contains(t, body, "amount must be a decimal amount.")
	})

	t.Run("the audit page narrows by who", func(t *testing.T) {
		_, body := b.get("/admin/audit?actor_type=admin&actor_id=" + h.admin.ID)
		assert.Contains(t, body, "ledger.house_adjustment.create")
		assert.Contains(t, body, `<option value="admin" selected>`)
		_, body = b.get("/admin/audit?actor_type=api_key")
		assert.Contains(t, body, "Nothing matches.", "everything so far was a person")
	})

	t.Run("a webhook endpoint, its secret shown once, then rotated", func(t *testing.T) {
		resp, _ := b.post("/admin/webhooks", url.Values{"url": {"https://example.com/hooks"}, "events": {"trade.executed, withdrawal.state_changed"}, "label": {"acme"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		loc := resp.Header.Get("Location")
		require.True(t, strings.HasPrefix(loc, "/admin/webhooks?reveal="), loc)
		_, body := b.get(loc)
		secret := regexp.MustCompile(`<pre class="mono">([0-9a-f]{64})</pre>`).FindStringSubmatch(body)
		require.Len(t, secret, 2, "the secret is on the page once")
		assert.Contains(t, body, "only time it is shown")
		assert.Contains(t, body, "acme")
		_, body = b.get(loc)
		assert.NotContains(t, body, secret[1], "and never again")
		assert.NotContains(t, body, "only time it is shown")

		id := regexp.MustCompile(`/admin/webhooks\?endpoint=([0-9a-f-]{36})`).FindStringSubmatch(body)
		require.Len(t, id, 2)
		_, body = b.get("/admin/webhooks?endpoint=" + id[1])
		assert.Contains(t, body, "Deliveries to acme")
		assert.Contains(t, body, "Nothing delivered yet.")

		resp, _ = b.post("/admin/webhooks/"+id[1]+"/rotate-secret", url.Values{"grace_hours": {"48"}, "reason": {"quarterly"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		_, body = b.get(resp.Header.Get("Location"))
		rotated := regexp.MustCompile(`<pre class="mono">([0-9a-f]{64})</pre>`).FindStringSubmatch(body)
		require.Len(t, rotated, 2)
		assert.NotEqual(t, secret[1], rotated[1])
		assert.Contains(t, body, "Deliveries carry both signatures until")

		resp, _ = b.post("/admin/webhooks/"+id[1]+"/status", url.Values{"status": {"disabled"}, "reason": {"paused"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		_, body = b.get("/admin/webhooks")
		assert.Contains(t, body, "Whatever was queued for it was dropped.")
		assert.Contains(t, body, `class="badge bad">disabled<`)

		resp, _ = b.post("/admin/webhooks/"+id[1], url.Values{"url": {"not a url"}, "events": {"trade.executed"}, "reason": {"x"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		_, body = b.get("/admin/webhooks")
		assert.Contains(t, body, `class="flash err"`, "an invalid edit is refused with a message")
	})

	t.Run("a withdrawal that does not exist", func(t *testing.T) {
		_, body := b.get("/admin/withdrawals?id=00000000-0000-0000-0000-000000000000")
		assert.Contains(t, body, "No withdrawal 00000000-0000-0000-0000-000000000000.")
		resp, _ := b.post("/admin/withdrawals/00000000-0000-0000-0000-000000000000/review", url.Values{"decision": {"approve"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		_, body = b.get("/admin/withdrawals")
		assert.Contains(t, body, "That withdrawal does not exist.")
	})
}
