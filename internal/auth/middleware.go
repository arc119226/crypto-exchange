package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// maxSignedBody bounds what the API-key path buffers to verify a signature.
const maxSignedBody = 1 << 20

// ErrorWriter renders an authentication failure in the API's error format
// (the API passes its RFC 7807 writer; auth cannot import api).
type ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, title, detail string)

// Authenticate resolves the caller: a `Authorization: Bearer <jwt>` header
// or the X-API-KEY / X-API-TIMESTAMP / X-API-SIGNATURE trio. A valid
// credential stores the Principal in the context; an invalid one is a 401
// (403 for a key used from a disallowed IP); no credential passes through
// unauthenticated and the handler decides whether the route needs one.
func (s *Service) Authenticate(onError ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var (
				p   Principal
				err error
			)
			switch {
			case r.Header.Get("Authorization") != "":
				token, ok := bearerToken(r.Header.Get("Authorization"))
				if !ok {
					w.Header().Set("WWW-Authenticate", `Bearer realm="exchange", error="invalid_request"`)
					onError(w, r, http.StatusUnauthorized, "Unauthorized", "Authorization header must be `Bearer <token>`")
					return
				}
				p, err = s.VerifyAccessToken(token)
				if err != nil {
					w.Header().Set("WWW-Authenticate", `Bearer realm="exchange", error="invalid_token"`)
					onError(w, r, http.StatusUnauthorized, "Unauthorized", "invalid or expired access token")
					return
				}
			case r.Header.Get(HeaderAPIKey) != "":
				body, rerr := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
				if rerr != nil || len(body) > maxSignedBody {
					onError(w, r, http.StatusRequestEntityTooLarge, "Payload Too Large", "signed request bodies are limited to 1 MiB")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				p, err = s.VerifyAPIKeyRequest(r.Context(), APIKeyRequest{
					KeyID: r.Header.Get(HeaderAPIKey), Timestamp: r.Header.Get(HeaderAPITimestamp), Signature: r.Header.Get(HeaderAPISignature),
					Method: r.Method, RequestURI: r.URL.RequestURI(), Body: body, IP: ClientIP(r),
				})
				if err != nil {
					if errors.Is(err, ErrForbidden) {
						onError(w, r, http.StatusForbidden, "Forbidden", "api key not allowed from this address")
						return
					}
					if errors.Is(err, ErrInvalidCredentials) {
						onError(w, r, http.StatusUnauthorized, "Unauthorized", strings.TrimPrefix(err.Error(), ErrInvalidCredentials.Error()+": "))
						return
					}
					onError(w, r, http.StatusInternalServerError, "Internal Server Error", "")
					return
				}
			default:
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func bearerToken(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// ClientIP returns the peer address without the port. Reverse proxies are
// out of scope for v1 (docs/plan-v1.0.md §18), so X-Forwarded-For is not
// trusted.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseIP(s string) (net.IP, error) {
	if ip := net.ParseIP(s); ip != nil {
		return ip, nil
	}
	if _, _, err := net.ParseCIDR(s); err == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("not an ip or cidr: %s", s)
}

// ipAllowed matches the client address against IPs and CIDRs.
func ipAllowed(client string, allow []string) bool {
	ip := net.ParseIP(client)
	if ip == nil {
		return false
	}
	for _, a := range allow {
		if strings.Contains(a, "/") {
			if _, n, err := net.ParseCIDR(a); err == nil && n.Contains(ip) {
				return true
			}
			continue
		}
		if other := net.ParseIP(a); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}
