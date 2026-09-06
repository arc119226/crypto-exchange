package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
)

func toSession(s auth.Session) gen.Session {
	return gen.Session{
		UserID: s.UserID, AccountID: s.AccountID, Role: gen.SessionRole(s.Role),
		AccessToken: s.AccessToken, TokenType: "Bearer", ExpiresIn: int64(s.ExpiresIn.Seconds()), ExpiresAt: s.ExpiresAt,
		RefreshToken: s.RefreshToken,
	}
}

func toAPIKey(k auth.APIKey) gen.APIKey {
	scopes := make([]gen.Scope, 0, len(k.Scopes))
	for _, s := range k.Scopes {
		scopes = append(scopes, gen.Scope(s))
	}
	ips := k.IPAllowlist
	if ips == nil {
		ips = []string{}
	}
	return gen.APIKey{ID: k.ID, KeyID: k.KeyID, Label: k.Label, Scopes: scopes, IPAllowlist: ips, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

// Register implements POST /v1/auth/register.
func (h *Handler) Register(ctx context.Context, req gen.RegisterRequestObject) (gen.RegisterResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return gen.Register400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "missing body")}, nil
	}
	ip := reqInfo(ctx).ip
	if d, err := h.allow(ctx, "login:ip:"+ip, h.d.Limits.LoginPerIP); err != nil {
		return nil, err
	} else if !d.Allowed {
		return gen.Register429ApplicationProblemPlusJSONResponse{TooManyRequestsApplicationProblemPlusJSONResponse: h.tooMany(ctx, d)}, nil
	}
	sess, err := h.d.Auth.Register(ctx, string(req.Body.Email), req.Body.Password, ip)
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		return gen.Register409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: h.conflict(ctx, "email already registered")}, nil
	case errors.Is(err, auth.ErrWeakPassword):
		return gen.Register422ApplicationProblemPlusJSONResponse{UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx, err.Error())}, nil
	case errors.Is(err, auth.ErrInvalidInput):
		return gen.Register400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, strings.TrimPrefix(err.Error(), "auth: "))}, nil
	case err != nil:
		return nil, fmt.Errorf("register: %w", err)
	}
	return gen.Register201JSONResponse(toSession(sess)), nil
}

// Login implements POST /v1/auth/login. Both limits are consumed by every
// attempt so a burst of failures locks the account key for the window.
func (h *Handler) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	if req.Body == nil {
		return gen.Login400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "missing body")}, nil
	}
	ip := reqInfo(ctx).ip
	email := strings.ToLower(strings.TrimSpace(string(req.Body.Email)))
	if d, err := h.allow(ctx, "login:ip:"+ip, h.d.Limits.LoginPerIP); err != nil {
		return nil, err
	} else if !d.Allowed {
		return gen.Login429ApplicationProblemPlusJSONResponse{TooManyRequestsApplicationProblemPlusJSONResponse: h.tooMany(ctx, d)}, nil
	}
	if d, err := h.allow(ctx, "login:acct:"+email, h.d.Limits.LoginPerAccount); err != nil {
		return nil, err
	} else if !d.Allowed {
		return gen.Login429ApplicationProblemPlusJSONResponse{TooManyRequestsApplicationProblemPlusJSONResponse: h.tooMany(ctx, d)}, nil
	}
	sess, err := h.d.Auth.Login(ctx, email, req.Body.Password, ip)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return gen.Login401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: gen.UnauthorizedApplicationProblemPlusJSONResponse(h.problem(ctx, 401, "Unauthorized", "invalid email or password"))}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	return gen.Login200JSONResponse(toSession(sess)), nil
}

// Refresh implements POST /v1/auth/refresh.
func (h *Handler) Refresh(ctx context.Context, req gen.RefreshRequestObject) (gen.RefreshResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	if req.Body == nil || req.Body.RefreshToken == "" {
		return gen.Refresh400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "refresh_token required")}, nil
	}
	sess, err := h.d.Auth.Refresh(ctx, req.Body.RefreshToken)
	if errors.Is(err, auth.ErrInvalidToken) {
		return gen.Refresh401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: gen.UnauthorizedApplicationProblemPlusJSONResponse(h.problem(ctx, 401, "Unauthorized", "invalid, expired or already used refresh token"))}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	return gen.Refresh200JSONResponse(toSession(sess)), nil
}

// Logout implements POST /v1/auth/logout.
func (h *Handler) Logout(ctx context.Context, req gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	if req.Body == nil || req.Body.RefreshToken == "" {
		return gen.Logout400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "refresh_token required")}, nil
	}
	if err := h.d.Auth.Logout(ctx, req.Body.RefreshToken); err != nil {
		return nil, fmt.Errorf("logout: %w", err)
	}
	return gen.Logout204Response{}, nil
}

// GetJWKS implements GET /.well-known/jwks.json.
func (h *Handler) GetJWKS(_ context.Context, _ gen.GetJWKSRequestObject) (gen.GetJWKSResponseObject, error) {
	if h.d.Auth == nil || h.d.Auth.Signer() == nil {
		return nil, errUnavailable
	}
	b, err := h.d.Auth.Signer().JWKS()
	if err != nil {
		return nil, err
	}
	var set gen.JWKS
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	return gen.GetJWKS200JSONResponse(set), nil
}

// ListAPIKeys implements GET /v1/api-keys.
func (h *Handler) ListAPIKeys(ctx context.Context, _ gen.ListAPIKeysRequestObject) (gen.ListAPIKeysResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListAPIKeys401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	keys, err := h.d.Auth.ListAPIKeys(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]gen.APIKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKey(k))
	}
	return gen.ListAPIKeys200JSONResponse(gen.APIKeyList{APIKeys: out}), nil
}

// CreateAPIKey implements POST /v1/api-keys (sessions only).
func (h *Handler) CreateAPIKey(ctx context.Context, req gen.CreateAPIKeyRequestObject) (gen.CreateAPIKeyResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	p, err := principal(ctx)
	if err != nil {
		return gen.CreateAPIKey401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	if p.Method != auth.MethodJWT {
		return gen.CreateAPIKey403ApplicationProblemPlusJSONResponse{ForbiddenApplicationProblemPlusJSONResponse: h.forbidden(ctx, "API keys can only be created from a session")}, nil
	}
	if req.Body == nil {
		return gen.CreateAPIKey400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "missing body")}, nil
	}
	scopes := make([]string, 0, len(req.Body.Scopes))
	for _, s := range req.Body.Scopes {
		scopes = append(scopes, string(s))
	}
	var label string
	if req.Body.Label != nil {
		label = *req.Body.Label
	}
	var ips []string
	if req.Body.IPAllowlist != nil {
		ips = *req.Body.IPAllowlist
	}
	key, secret, err := h.d.Auth.CreateAPIKey(ctx, p.UserID, label, scopes, ips)
	if errors.Is(err, auth.ErrInvalidInput) {
		return gen.CreateAPIKey400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, strings.TrimPrefix(err.Error(), "auth: "))}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	k := toAPIKey(key)
	return gen.CreateAPIKey201JSONResponse(gen.CreatedAPIKey{
		ID: k.ID, KeyID: k.KeyID, Label: k.Label, Scopes: k.Scopes, IPAllowlist: k.IPAllowlist, CreatedAt: k.CreatedAt, Secret: secret,
	}), nil
}

// RevokeAPIKey implements DELETE /v1/api-keys/{id} (sessions only).
func (h *Handler) RevokeAPIKey(ctx context.Context, req gen.RevokeAPIKeyRequestObject) (gen.RevokeAPIKeyResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	p, err := principal(ctx)
	if err != nil {
		return gen.RevokeAPIKey401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	if p.Method != auth.MethodJWT {
		return gen.RevokeAPIKey403ApplicationProblemPlusJSONResponse{ForbiddenApplicationProblemPlusJSONResponse: h.forbidden(ctx, "API keys can only be revoked from a session")}, nil
	}
	err = h.d.Auth.RevokeAPIKey(ctx, p.UserID, req.ID)
	if errors.Is(err, auth.ErrNotFound) {
		return gen.RevokeAPIKey404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "api key "+req.ID+" does not exist or is already revoked")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("revoke api key: %w", err)
	}
	return gen.RevokeAPIKey204Response{}, nil
}
