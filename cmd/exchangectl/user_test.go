package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sessionJSON = `{"user_id":"u-1","account_id":"a-1","role":"user","access_token":"fresh-token",` +
	`"token_type":"Bearer","expires_in":900,"expires_at":"2026-01-01T00:00:00Z","refresh_token":"fresh-refresh"}`

// authAPI answers the way the server really does: internal/auth/middleware.go
// rejects a credential that is present but invalid before any handler runs,
// whether or not the route needs one. /v1/auth/* needs none -- the OpenAPI
// spec gives register, login, refresh and logout no `security` -- so the only
// way to reach them with a stale token in the environment is not to send it.
func authAPI(t *testing.T) *httptest.Server {
	t.Helper()
	reject := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-API-KEY") == "" {
			return false
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,` +
			`"detail":"invalid or expired access token","instance":"` + r.URL.Path + `","correlation_id":"corr-1"}`))
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		if reject(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(sessionJSON))
	})
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if reject(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sessionJSON))
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if reject(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// GET /v1/account does need the credential, and is here to prove the
	// anonymous client is confined to the three commands that exchange one.
	mux.HandleFunc("GET /v1/account", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer expired-token" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,` +
				`"detail":"no credential was sent","instance":"/v1/account","correlation_id":"corr-2"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u-1","account_id":"a-1","email":"a@example.com",` +
			`"role":"user","kyc_level":0,"status":"active"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An expired token in the environment must not block the one command that
// would replace it. The credential is attached by a request editor on every
// request, so `EXCHANGE_TOKEN` left over from yesterday makes `user login`
// answer 401 -- and the operator cannot log in without first knowing to unset
// a variable nothing told them about.
func TestAuthCommandsIgnoreAStaleCredential(t *testing.T) {
	srv := authAPI(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"login", []string{"user", "login", "--email", "a@example.com", "--password", "password1"}},
		{"register", []string{"user", "register", "--email", "a@example.com", "--password", "password1"}},
		{"logout", []string{"user", "logout", "--refresh-token", "stale-refresh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--base-url", srv.URL, "--token", "expired-token"}, tc.args...)
			out, err := run(t, args...)
			require.NoError(t, err, out)
		})
	}

	t.Run("with an api key instead", func(t *testing.T) {
		out, err := run(t, "--base-url", srv.URL, "--api-key", "k", "--api-secret", "s",
			"user", "login", "--email", "a@example.com", "--password", "password1")
		require.NoError(t, err, out)
	})
}

// The session it prints has to be the new one, not an echo of the request.
func TestLoginPrintsTheNewSession(t *testing.T) {
	srv := authAPI(t)
	out, err := run(t, "--base-url", srv.URL, "--token", "expired-token", "--output", "json",
		"user", "login", "--email", "a@example.com", "--password", "password1")
	require.NoError(t, err, out)
	var got struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "fresh-token", got.AccessToken)
	assert.Equal(t, "fresh-refresh", got.RefreshToken)
}

// Every other command still authenticates: dropping the credential is scoped
// to the routes that exist to hand one out.
func TestOtherCommandsStillSendTheCredential(t *testing.T) {
	srv := authAPI(t)
	out, err := run(t, "--base-url", srv.URL, "--token", "expired-token", "user", "me")
	require.NoError(t, err, out)
	assert.Contains(t, out, "a@example.com")
}
