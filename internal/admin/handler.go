package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// Handler implements gen.StrictServerInterface.
type Handler struct {
	pool     *pgxpool.Pool
	ledger   *ledger.Service
	audit    *audit.Recorder
	registry *registry.Store
	tenant   string
	// chainID is the chain reconciliation reports are read for. Zero until a
	// deployment says otherwise, which matches the single-chain assumption
	// everything else in Phase 4 makes.
	chainID int64
	// withdrawals is the review queue; nil in a deployment without the chain
	// tables, which turns the two withdrawal endpoints into 500s rather than
	// pretending the queue is empty.
	withdrawals *withdrawal.Reviewer
	// webhooks is the outbound-webhook configuration; nil in a deployment
	// without the webhook tables, which turns those endpoints into 500s
	// rather than reporting that no customer has subscribed to anything.
	webhooks *webhook.Store
	// deposits reads chain.deposits; nil without a chain.
	deposits *deposit.Reader
	// users is the directory and the two edits an operator makes to a
	// person: KYC level and status. The same auth.Service the pages log in
	// with; nil turns the user endpoints into 500s.
	users *auth.Service
}

var _ gen.StrictServerInterface = (*Handler)(nil)

// NewHandler wires the admin API to the ledger, the registry and the audit
// trail. The pool must carry a role allowed to write all three (ex_admin or
// ex_all).
func NewHandler(pool *pgxpool.Pool, l *ledger.Service, r *registry.Store, a *audit.Recorder, tenant string) *Handler {
	if tenant == "" {
		tenant = "default"
	}
	return &Handler{pool: pool, ledger: l, registry: r, audit: a, tenant: tenant}
}

// WithChainID names the chain whose reconciliation reports this handler reads.
func (h *Handler) WithChainID(id int64) *Handler { h.chainID = id; return h }

// WithWithdrawals enables the withdrawal review queue.
func (h *Handler) WithWithdrawals(r *withdrawal.Reviewer) *Handler {
	h.withdrawals = r
	return h
}

// WithWebhooks enables the outbound-webhook endpoints.
func (h *Handler) WithWebhooks(s *webhook.Store) *Handler {
	h.webhooks = s
	return h
}

// WithDeposits enables the deposit list and the dashboard's deposit count.
func (h *Handler) WithDeposits(r *deposit.Reader) *Handler {
	h.deposits = r
	return h
}

// WithUsers enables the user directory and edits.
func (h *Handler) WithUsers(s *auth.Service) *Handler {
	h.users = s
	return h
}

func (h *Handler) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func notFound(ctx context.Context, instance, detail string) gen.NotFoundApplicationProblemPlusJSONResponse {
	return gen.NotFoundApplicationProblemPlusJSONResponse(NewProblem(ctx, http.StatusNotFound, "Not Found", detail, instance))
}

func badRequest(ctx context.Context, instance, detail string) gen.BadRequestApplicationProblemPlusJSONResponse {
	return gen.BadRequestApplicationProblemPlusJSONResponse(NewProblem(ctx, http.StatusBadRequest, "Bad Request", detail, instance))
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

// ListAccounts implements GET /admin/v1/accounts.
func (h *Handler) ListAccounts(ctx context.Context, req gen.ListAccountsRequestObject) (gen.ListAccountsResponseObject, error) {
	kind := ledger.AccountKind("")
	if req.Params.Kind != nil {
		kind = ledger.AccountKind(*req.Params.Kind)
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	accounts, err := h.ledger.ListAccounts(ctx, kind, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Account, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, toAccount(a))
	}
	return gen.ListAccounts200JSONResponse(gen.AccountList{Accounts: out}), nil
}

// CreateAccount implements POST /admin/v1/accounts.
func (h *Handler) CreateAccount(ctx context.Context, req gen.CreateAccountRequestObject) (gen.CreateAccountResponseObject, error) {
	if req.Body == nil {
		return gen.CreateAccount400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, "/admin/v1/accounts", "missing body")}, nil
	}
	created, err := h.createAccount(ctx, req.Body.OwnerUserID)
	if err != nil {
		return nil, err
	}
	return gen.CreateAccount201JSONResponse(toAccount(created)), nil
}

// GetAccount implements GET /admin/v1/accounts/{id}.
func (h *Handler) GetAccount(ctx context.Context, req gen.GetAccountRequestObject) (gen.GetAccountResponseObject, error) {
	a, err := h.ledger.Account(ctx, req.ID)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		return gen.GetAccount404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/accounts/"+req.ID, "account "+req.ID+" does not exist")}, nil
	}
	if err != nil {
		return nil, err
	}
	return gen.GetAccount200JSONResponse(toAccount(a)), nil
}

// SetAccountStatus implements PUT /admin/v1/accounts/{id}/status.
func (h *Handler) SetAccountStatus(ctx context.Context, req gen.SetAccountStatusRequestObject) (gen.SetAccountStatusResponseObject, error) {
	instance := "/admin/v1/accounts/" + req.ID + "/status"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetAccountStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "status and reason are required")}, nil
	}
	after, err := h.setAccountStatus(ctx, req.ID, string(req.Body.Status), req.Body.Reason)
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound):
		return gen.SetAccountStatus404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "account "+req.ID+" does not exist")}, nil
	case errors.Is(err, ledger.ErrInvalidEntry):
		return gen.SetAccountStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error())}, nil
	case err != nil:
		return nil, err
	}
	return gen.SetAccountStatus200JSONResponse(toAccount(after)), nil
}

// GetAccountBalances implements GET /admin/v1/accounts/{id}/balances.
func (h *Handler) GetAccountBalances(ctx context.Context, req gen.GetAccountBalancesRequestObject) (gen.GetAccountBalancesResponseObject, error) {
	balances, err := h.ledger.Balances(ctx, req.ID)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		return gen.GetAccountBalances404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/accounts/"+req.ID+"/balances", "account "+req.ID+" does not exist")}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]gen.Balance, 0, len(balances))
	for _, b := range balances {
		out = append(out, gen.Balance{Asset: b.Asset, Available: b.Available, Hold: b.Hold, Total: b.Total()})
	}
	return gen.GetAccountBalances200JSONResponse(gen.BalanceList{AccountID: req.ID, Balances: out}), nil
}

// GetTrialBalance implements GET /admin/v1/ledger/trial-balance.
func (h *Handler) GetTrialBalance(ctx context.Context, _ gen.GetTrialBalanceRequestObject) (gen.GetTrialBalanceResponseObject, error) {
	lines, err := h.ledger.TrialBalance(ctx)
	if err != nil {
		return nil, err
	}
	house, err := h.ledger.HouseBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.TrialBalance{Balanced: true, Lines: make([]gen.TrialBalanceLine, 0, len(lines)), House: make([]gen.HouseBalance, 0, len(house))}
	for _, l := range lines {
		if !l.Diff.IsZero() {
			out.Balanced = false
		}
		out.Lines = append(out.Lines, gen.TrialBalanceLine{Asset: l.Asset, Debits: l.Debits, Credits: l.Credits, Diff: l.Diff})
	}
	for _, hb := range house {
		out.House = append(out.House, gen.HouseBalance{Code: gen.HouseCode(hb.Code), Type: gen.HouseBalanceType(hb.Type), Asset: hb.Asset, Balance: hb.Balance})
	}
	return gen.GetTrialBalance200JSONResponse(out), nil
}

// ListEntries implements GET /admin/v1/ledger/entries.
func (h *Handler) ListEntries(ctx context.Context, req gen.ListEntriesRequestObject) (gen.ListEntriesResponseObject, error) {
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	f := ledger.EntriesFilter{AccountID: deref(req.Params.AccountID), RefType: deref(req.Params.RefType), RefID: deref(req.Params.RefID), Limit: limit, Offset: offset}
	entries, err := h.ledger.Entries(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]gen.JournalEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, toEntry(e))
	}
	return gen.ListEntries200JSONResponse(gen.JournalEntryList{Entries: out}), nil
}

// CreateAdjustment implements POST /admin/v1/ledger/adjustments.
func (h *Handler) CreateAdjustment(ctx context.Context, req gen.CreateAdjustmentRequestObject) (gen.CreateAdjustmentResponseObject, error) {
	const instance = "/admin/v1/ledger/adjustments"
	if req.Body == nil {
		return gen.CreateAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "missing body")}, nil
	}
	b := req.Body
	if !b.Amount.IsPositive() {
		return gen.CreateAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "amount must be positive")}, nil
	}
	if b.Reason == "" {
		return gen.CreateAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "reason is required")}, nil
	}
	key := b.IdempotencyKey
	if key == "" {
		key = "adjust:" + randomID()
	}
	entry, replayed, err := h.createAdjustment(ctx, ledger.AdjustParams{
		AccountID: b.AccountID, Asset: b.Asset, Amount: b.Amount, Direction: ledger.Direction(b.Direction),
		Reason: b.Reason, IdempotencyKey: key,
	})
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound):
		return gen.CreateAdjustment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "account "+b.AccountID+" does not exist")}, nil
	case errors.Is(err, ledger.ErrInsufficient):
		return gen.CreateAdjustment422ApplicationProblemPlusJSONResponse(NewProblem(ctx, http.StatusUnprocessableEntity, "Insufficient Balance", err.Error(), instance)), nil
	case errors.Is(err, ledger.ErrReasonRequired), errors.Is(err, ledger.ErrInvalidEntry), errors.Is(err, ledger.ErrAccountKind):
		return gen.CreateAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error())}, nil
	case err != nil:
		return nil, err
	}
	if replayed {
		return gen.CreateAdjustment200JSONResponse(toEntry(entry)), nil
	}
	return gen.CreateAdjustment201JSONResponse(toEntry(entry)), nil
}

// ListAuditEvents implements GET /admin/v1/audit-events.
func (h *Handler) ListAuditEvents(ctx context.Context, req gen.ListAuditEventsRequestObject) (gen.ListAuditEventsResponseObject, error) {
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	actorType := ""
	if req.Params.ActorType != nil {
		actorType = string(*req.Params.ActorType)
	}
	events, err := h.audit.List(ctx, h.pool, audit.Filter{
		Action: deref(req.Params.Action), TargetType: deref(req.Params.TargetType), TargetID: deref(req.Params.TargetID),
		ActorType: actorType, ActorID: deref(req.Params.ActorID), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]gen.AuditEvent, 0, len(events))
	for _, e := range events {
		out = append(out, gen.AuditEvent{
			ID: e.ID, ActorType: gen.AuditEventActorType(e.ActorType), ActorID: e.ActorID, Action: e.Action,
			TargetType: e.TargetType, TargetID: e.TargetID, Before: rawJSON(e.Before), After: rawJSON(e.After),
			IP: e.IP, CorrelationID: e.CorrelationID, CreatedAt: e.CreatedAt,
		})
	}
	return gen.ListAuditEvents200JSONResponse(gen.AuditEventList{Events: out}), nil
}

func toAccount(a ledger.Account) gen.Account {
	out := gen.Account{ID: a.ID, Kind: gen.AccountKind(a.Kind), Status: gen.AccountStatus(a.Status), OwnerUserID: a.OwnerUserID, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	if a.HouseCode != "" {
		code := gen.HouseCode(a.HouseCode)
		out.HouseCode = &code
	}
	return out
}

func toEntry(e ledger.JournalEntry) gen.JournalEntry {
	postings := make([]gen.Posting, 0, len(e.Postings))
	for _, p := range e.Postings {
		postings = append(postings, gen.Posting{AccountID: p.AccountID, Asset: p.Asset, Bucket: gen.PostingBucket(p.Bucket), Direction: gen.PostingDirection(p.Direction), Amount: p.Amount})
	}
	return gen.JournalEntry{
		ID: e.ID, IdempotencyKey: e.IdempotencyKey, Kind: e.Kind, RefType: e.RefType, RefID: e.RefID, Reason: e.Reason,
		CorrelationID: e.CorrelationID, CreatedAt: e.CreatedAt, Postings: postings,
	}
}

func rawJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("admin: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
