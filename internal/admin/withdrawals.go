package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
)

// ListWithdrawalsForReview implements GET /admin/v1/withdrawals.
func (h *Handler) ListWithdrawalsForReview(ctx context.Context, req gen.ListWithdrawalsForReviewRequestObject) (gen.ListWithdrawalsForReviewResponseObject, error) {
	if h.withdrawals == nil {
		return nil, errors.New("admin: withdrawals are not enabled on this deployment")
	}
	limit, _ := page(req.Params.Limit, nil)
	rows, err := h.withdrawals.Pending(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list withdrawals for review: %w", err)
	}
	out := make([]gen.AdminWithdrawal, 0, len(rows))
	for _, w := range rows {
		out = append(out, toAdminWithdrawal(w))
	}
	return gen.ListWithdrawalsForReview200JSONResponse(gen.AdminWithdrawalList{Withdrawals: out}), nil
}

// ReviewWithdrawal implements POST /admin/v1/withdrawals/{id}/review.
//
// Approving marks the withdrawal for the chain worker; it does not lock funds
// and cannot sign. That is the point of the split (docs/plan-v1.0.md §6.4.2):
// the role that authorises a withdrawal is not the role that acts on it.
func (h *Handler) ReviewWithdrawal(ctx context.Context, req gen.ReviewWithdrawalRequestObject) (gen.ReviewWithdrawalResponseObject, error) {
	instance := "/admin/v1/withdrawals/" + req.ID + "/review"
	if h.withdrawals == nil {
		return nil, errors.New("admin: withdrawals are not enabled on this deployment")
	}
	if req.Body == nil {
		return gen.ReviewWithdrawal400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "a decision is required"),
		}, nil
	}
	if !req.Body.Decision.Valid() {
		return gen.ReviewWithdrawal400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "decision must be approve or reject"),
		}, nil
	}
	approve := req.Body.Decision == gen.WithdrawalReviewRequestDecisionApprove
	// A rejection with no reason leaves the user, and the next operator to
	// look at the row, with nothing to go on.
	note := ""
	if req.Body.Note != nil {
		note = *req.Body.Note
	}
	if !approve && note == "" {
		return gen.ReviewWithdrawal400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "a rejection must carry a note saying why"),
		}, nil
	}
	rec, err := h.withdrawals.Review(ctx, withdrawal.ReviewParams{
		ID: req.ID, Approve: approve, Note: note,
		// AdminID stays empty: the admin API authenticates with a static key,
		// so there is no user id to write into reviewed_by. The audit trail
		// records the actor, exactly as the other admin endpoints do.
		ActorType: audit.ActorAPIKey, ActorID: actorID,
	})
	switch {
	case errors.Is(err, withdrawal.ErrNotFound):
		return gen.ReviewWithdrawal404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "no withdrawal "+req.ID),
		}, nil
	case errors.Is(err, withdrawal.ErrNotReviewable):
		return gen.ReviewWithdrawal409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("review withdrawal %s: %w", req.ID, err)
	}
	return gen.ReviewWithdrawal200JSONResponse(toAdminWithdrawal(rec)), nil
}

func conflict(ctx context.Context, instance, detail string) gen.ConflictApplicationProblemPlusJSONResponse {
	return gen.ConflictApplicationProblemPlusJSONResponse(NewProblem(ctx, http.StatusConflict, "Conflict", detail, instance))
}

func toAdminWithdrawal(w withdrawal.Record) gen.AdminWithdrawal {
	out := gen.AdminWithdrawal{
		ID: w.ID, AccountID: w.AccountID, Asset: w.Asset, Amount: w.Amount,
		ToAddress: w.ToAddress, ChainID: w.ChainID, Status: w.Status,
		ReviewedAt: w.ReviewedAt,
		CreatedAt:  w.CreatedAt, UpdatedAt: &w.UpdatedAt,
	}
	if w.FailureReason != "" {
		out.FailureReason = &w.FailureReason
	}
	if w.ReviewNote != "" {
		out.ReviewNote = &w.ReviewNote
	}
	if w.ReviewedBy != "" {
		out.ReviewedBy = &w.ReviewedBy
	}
	return out
}
