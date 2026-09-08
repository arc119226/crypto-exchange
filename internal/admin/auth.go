package admin

import (
	"crypto/subtle"
	"net/http"
)

// APIKeyHeader carries the static admin API key that machine integrations
// present on /admin/v1. People use the back office instead, with a session
// (see session.go); scoped, HMAC-signed admin keys (docs/plan-v1.0.md §7.4)
// are not built yet, so this key is still the only machine credential.
const APIKeyHeader = "X-Admin-Api-Key" //nolint:gosec // a header name, not a credential

// RequireAPIKey rejects requests whose X-Admin-Api-Key does not match key
// (constant-time compare). An empty key disables the API with 503 rather
// than leaving it open.
func RequireAPIKey(key string) func(http.Handler) http.Handler {
	expected := []byte(key)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(expected) == 0 {
				WriteProblem(w, r, http.StatusServiceUnavailable, "Admin API Disabled", "ADMIN_API_KEY is not configured")
				return
			}
			got := []byte(r.Header.Get(APIKeyHeader))
			if len(got) == 0 || subtle.ConstantTimeCompare(got, expected) != 1 {
				w.Header().Set("WWW-Authenticate", `ApiKey realm="admin", header="`+APIKeyHeader+`"`)
				WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", "missing or invalid "+APIKeyHeader)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
