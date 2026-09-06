package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// GetAccount implements GET /v1/account.
func (h *Handler) GetAccount(ctx context.Context, _ gen.GetAccountRequestObject) (gen.GetAccountResponseObject, error) {
	if h.d.Auth == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.GetAccount401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	u, err := h.d.Auth.User(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return gen.GetAccount401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
		}
		return nil, fmt.Errorf("get account: %w", err)
	}
	return gen.GetAccount200JSONResponse(gen.Account{
		UserID: u.ID, AccountID: p.AccountID, Email: u.Email, Role: gen.AccountRole(u.Role),
		KycLevel: int32(u.KYCLevel), Status: gen.AccountStatus(u.Status), CreatedAt: u.CreatedAt, //nolint:gosec // 0..2
	}), nil
}

// ListBalances implements GET /v1/balances.
func (h *Handler) ListBalances(ctx context.Context, _ gen.ListBalancesRequestObject) (gen.ListBalancesResponseObject, error) {
	if h.d.Ledger == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListBalances401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	balances, err := h.d.Ledger.Balances(ctx, p.AccountID)
	if err != nil {
		return nil, fmt.Errorf("balances: %w", err)
	}
	out := make([]gen.Balance, 0, len(balances))
	for _, b := range balances {
		out = append(out, gen.Balance{Asset: b.Asset, Available: b.Available, Hold: b.Hold, Total: b.Total()})
	}
	return gen.ListBalances200JSONResponse(gen.BalanceList{Balances: out}), nil
}

// ListLedgerEntries implements GET /v1/ledger/entries: the caller's entries
// with only the caller's postings (counterparties stay private).
func (h *Handler) ListLedgerEntries(ctx context.Context, req gen.ListLedgerEntriesRequestObject) (gen.ListLedgerEntriesResponseObject, error) {
	if h.d.Ledger == nil {
		return nil, errUnavailable
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListLedgerEntries401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	entries, err := h.d.Ledger.Entries(ctx, ledger.EntriesFilter{AccountID: p.AccountID, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	out := make([]gen.LedgerEntry, 0, len(entries))
	for _, e := range entries {
		postings := make([]gen.Posting, 0, len(e.Postings))
		for _, po := range e.Postings {
			if po.AccountID != p.AccountID {
				continue
			}
			postings = append(postings, gen.Posting{Asset: po.Asset, Bucket: gen.PostingBucket(po.Bucket), Direction: gen.PostingDirection(po.Direction), Amount: po.Amount})
		}
		out = append(out, gen.LedgerEntry{ID: e.ID, Kind: e.Kind, RefType: e.RefType, RefID: e.RefID, Reason: e.Reason, CreatedAt: e.CreatedAt, Postings: postings})
	}
	return gen.ListLedgerEntries200JSONResponse(gen.LedgerEntryList{Entries: out}), nil
}

func page(limit *gen.Limit, offset *gen.Offset) (int32, int32) {
	l, o := int32(100), int32(0)
	if limit != nil {
		l = *limit
	}
	if offset != nil {
		o = *offset
	}
	return l, o
}
