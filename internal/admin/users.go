package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
)

// ListUsers implements GET /admin/v1/users.
func (h *Handler) ListUsers(ctx context.Context, req gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	if h.users == nil {
		return nil, errUsersDisabled
	}
	var f auth.UserFilter
	if req.Params.Email != nil {
		f.Email = *req.Params.Email
	}
	if req.Params.Status != nil {
		f.Status = string(*req.Params.Status)
	}
	if req.Params.Role != nil {
		f.Role = string(*req.Params.Role)
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	users, err := h.users.ListUsers(ctx, f, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]gen.User, 0, len(users))
	for _, u := range users {
		out = append(out, toUser(u))
	}
	return gen.ListUsers200JSONResponse(gen.UserList{Users: out}), nil
}

// GetUser implements GET /admin/v1/users/{id}.
func (h *Handler) GetUser(ctx context.Context, req gen.GetUserRequestObject) (gen.GetUserResponseObject, error) {
	if h.users == nil {
		return nil, errUsersDisabled
	}
	u, err := h.users.User(ctx, req.ID)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return gen.GetUser404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/users/"+req.ID, "user "+req.ID+" does not exist"),
		}, nil
	case err != nil:
		return nil, err
	}
	return gen.GetUser200JSONResponse(toUser(u)), nil
}

// SetUserKycLevel implements PUT /admin/v1/users/{id}/kyc-level.
func (h *Handler) SetUserKycLevel(ctx context.Context, req gen.SetUserKycLevelRequestObject) (gen.SetUserKycLevelResponseObject, error) {
	instance := "/admin/v1/users/" + req.ID + "/kyc-level"
	if h.users == nil {
		return nil, errUsersDisabled
	}
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetUserKycLevel400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "kyc_level and reason are required"),
		}, nil
	}
	u, err := h.setUserKYCLevel(ctx, req.ID, req.Body.KycLevel, req.Body.Reason)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return gen.SetUserKycLevel404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "user "+req.ID+" does not exist"),
		}, nil
	case errors.Is(err, auth.ErrInvalidInput):
		return gen.SetUserKycLevel400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("set kyc level of %s: %w", req.ID, err)
	}
	return gen.SetUserKycLevel200JSONResponse(toUser(u)), nil
}

// SetUserStatus implements PUT /admin/v1/users/{id}/status.
func (h *Handler) SetUserStatus(ctx context.Context, req gen.SetUserStatusRequestObject) (gen.SetUserStatusResponseObject, error) {
	instance := "/admin/v1/users/" + req.ID + "/status"
	if h.users == nil {
		return nil, errUsersDisabled
	}
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetUserStatus400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "status and reason are required"),
		}, nil
	}
	u, err := h.setUserStatus(ctx, req.ID, string(req.Body.Status), req.Body.Reason)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		return gen.SetUserStatus404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "user "+req.ID+" does not exist"),
		}, nil
	case errors.Is(err, auth.ErrInvalidInput):
		return gen.SetUserStatus400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case errors.Is(err, auth.ErrLastAdmin):
		return gen.SetUserStatus409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance,
				"this is the last active administrator; freezing them would lock everyone out of the back office"),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("set status of %s: %w", req.ID, err)
	}
	return gen.SetUserStatus200JSONResponse(toUser(u)), nil
}

var errUsersDisabled = errors.New("admin: the user directory is not wired on this deployment")

func toUser(u auth.User) gen.User {
	return gen.User{
		ID: u.ID, Email: u.Email, Role: gen.UserRole(u.Role), KycLevel: u.KYCLevel, Status: gen.UserStatus(u.Status),
		TotpEnabled: u.TOTPEnabled, Version: u.Version, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}
