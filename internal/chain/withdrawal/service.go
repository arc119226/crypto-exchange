package withdrawal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Service records withdrawal requests. It runs in the api role, which may
// INSERT and read but holds no UPDATE on chain.withdrawals at all: everything
// that moves a withdrawal towards being signed happens in the chain role.
type Service struct {
	db      *pgxpool.Pool
	tenant  string
	chainID int64
	reg     registry.Reader
	ledger  *ledger.Service
	audit   *audit.Recorder
}

// NewService builds the api-side service.
func NewService(db *pgxpool.Pool, tenant string, chainID int64, reg registry.Reader, l *ledger.Service, rec *audit.Recorder) *Service {
	return &Service{db: db, tenant: tenant, chainID: chainID, reg: reg, ledger: l, audit: rec}
}

// CreateParams is one withdrawal request.
type CreateParams struct {
	AccountID string
	UserID    string
	Asset     string
	Amount    money.Amount
	ToAddress string
	// IdempotencyKey is the client's Idempotency-Key header.
	IdempotencyKey string
	IP             string
	CorrelationID  string
}

// Create records a withdrawal in status requested (docs/plan-v1.0.md §6.4.2).
//
// Idempotency follows what trading already does for client_order_id rather
// than the key/response cache §6.4.2 sketches: the key lives on the withdrawal
// row under a unique index, a replay of the same request returns that row, and
// the same key with a different request is ErrIdempotencyMismatch. The
// difference matters twice — there is no 24 h window after which the same key
// would quietly create a second withdrawal, and a replay answers with the
// withdrawal's current state rather than a frozen copy of the first response.
func (s *Service) Create(ctx context.Context, p CreateParams) (CreateResult, error) {
	asset, err := s.reg.GetAsset(ctx, s.tenant, p.Asset)
	if errors.Is(err, registry.ErrNotFound) {
		return CreateResult{}, fmt.Errorf("%w: unknown asset %s", ErrInvalid, p.Asset)
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("withdrawal: asset %s: %w", p.Asset, err)
	}
	if p.IdempotencyKey == "" {
		return CreateResult{}, fmt.Errorf("%w: an Idempotency-Key is required", ErrInvalid)
	}
	to, err := normaliseAddress(p.ToAddress)
	if err != nil {
		return CreateResult{}, err
	}
	if !p.Amount.IsPositive() {
		return CreateResult{}, fmt.Errorf("%w: amount must be positive", ErrInvalid)
	}
	// Reject here what the policy would reject anyway, so an obviously
	// impossible request never reaches the queue at all (§6.4.2 "basic
	// validation"). The policy checks these again: the api's copy is a
	// courtesy, not the authority.
	if !asset.WithdrawEnabled || asset.Status != registry.AssetActive {
		return CreateResult{}, fmt.Errorf("%w: %s withdrawals are disabled", ErrInvalid, asset.Symbol)
	}
	if p.Amount.Cmp(asset.MinWithdrawal) < 0 {
		return CreateResult{}, fmt.Errorf("%w: %s minimum withdrawal is %s", ErrInvalid, asset.Symbol, asset.MinWithdrawal)
	}
	if p.Amount.Scale() > asset.Scale {
		return CreateResult{}, fmt.Errorf("%w: %s has %d decimals, %s has more", ErrInvalid, asset.Symbol, asset.Scale, p.Amount)
	}

	hash := requestHash(asset.Symbol, p.Amount, to, s.chainID)
	if existing, err := s.byKey(ctx, p.AccountID, p.IdempotencyKey); err == nil {
		if existing.RequestHash != hash {
			return CreateResult{}, ErrIdempotencyMismatch
		}
		rec, err := recordFrom(existing)
		return CreateResult{Record: rec, Replayed: true}, err
	} else if !errors.Is(err, ErrNotFound) {
		return CreateResult{}, err
	}

	// An available-balance pre-check. It is advisory: the funds are not held
	// until the chain worker locks them, so between here and there the balance
	// can move. Telling a user now beats a withdrawal that fails minutes later.
	// (Balance answers zero for an account that has never held the asset, so
	// there is no not-found case to fold in here.)
	balance, err := s.ledger.Balance(ctx, p.AccountID, asset.Symbol)
	if err != nil {
		return CreateResult{}, fmt.Errorf("withdrawal: balance of %s: %w", p.AccountID, err)
	}
	if balance.Available.Cmp(p.Amount) < 0 {
		return CreateResult{}, fmt.Errorf("%w: available %s %s is less than %s",
			ErrInvalid, balance.Available, asset.Symbol, p.Amount)
	}

	var out CreateResult
	err = inTx(ctx, s.db, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).InsertWithdrawal(ctx, sqlcgen.InsertWithdrawalParams{
			TenantID: s.tenant, AccountID: p.AccountID, Asset: asset.Symbol,
			Amount: pg.NumericFromAmount(p.Amount), ToAddress: to, ChainID: s.chainID,
			IdempotencyKey: p.IdempotencyKey, RequestHash: hash,
			CorrelationID: optString(p.CorrelationID),
		})
		if err != nil {
			// Two requests with the same key at once: one inserts, the other
			// loses the unique index. The loser re-reads rather than failing,
			// so a retrying client cannot be punished for being fast.
			if isUniqueViolation(err) {
				existing, gerr := s.byKeyTx(ctx, tx, p.AccountID, p.IdempotencyKey)
				if gerr != nil {
					return gerr
				}
				if existing.RequestHash != hash {
					return ErrIdempotencyMismatch
				}
				rec, cerr := recordFrom(existing)
				out = CreateResult{Record: rec, Replayed: true}
				return cerr
			}
			return fmt.Errorf("withdrawal: insert: %w", err)
		}
		if err := s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorUser, ActorID: p.UserID, Action: "withdrawal.request",
			TargetType: "withdrawal", TargetID: row.ID,
			After: map[string]any{"asset": row.Asset, "amount": p.Amount.String(), "to_address": to, "status": row.Status},
			IP:    p.IP, CorrelationID: p.CorrelationID,
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		if err := emit(ctx, tx, s.ledger, s.tenant, EventRequested, row, "", ""); err != nil {
			return err
		}
		rec, err := recordFrom(row)
		out = CreateResult{Record: rec}
		return err
	})
	return out, err
}

// Get returns one withdrawal of an account.
func (s *Service) Get(ctx context.Context, accountID, id string) (Record, error) {
	row, err := sqlcgen.New(s.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: s.tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("withdrawal: get %s: %w", id, err)
	}
	// Not found rather than forbidden: the caller must not learn that a
	// withdrawal it cannot see exists.
	if row.AccountID != accountID {
		return Record{}, ErrNotFound
	}
	return recordFrom(row)
}

// List returns an account's withdrawals, newest first.
func (s *Service) List(ctx context.Context, accountID string, limit int32) ([]Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := sqlcgen.New(s.db).ListWithdrawalsByAccount(ctx, sqlcgen.ListWithdrawalsByAccountParams{
		TenantID: s.tenant, AccountID: accountID, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("withdrawal: list: %w", err)
	}
	return records(rows)
}

func (s *Service) byKey(ctx context.Context, accountID, key string) (sqlcgen.ChainWithdrawal, error) {
	row, err := sqlcgen.New(s.db).GetWithdrawalByIdempotencyKey(ctx, sqlcgen.GetWithdrawalByIdempotencyKeyParams{
		TenantID: s.tenant, AccountID: accountID, IdempotencyKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, fmt.Errorf("withdrawal: by idempotency key: %w", err)
	}
	return row, nil
}

func (s *Service) byKeyTx(ctx context.Context, tx pgx.Tx, accountID, key string) (sqlcgen.ChainWithdrawal, error) {
	row, err := sqlcgen.New(tx).GetWithdrawalByIdempotencyKey(ctx, sqlcgen.GetWithdrawalByIdempotencyKeyParams{
		TenantID: s.tenant, AccountID: accountID, IdempotencyKey: key,
	})
	if err != nil {
		return row, fmt.Errorf("withdrawal: by idempotency key: %w", err)
	}
	return row, nil
}

// requestHash is what an Idempotency-Key is a key for. Everything a client
// could have meant differently is in it; the correlation id and the account
// are not, the first because it is metadata and the second because the key is
// already scoped to the account.
func requestHash(asset string, amount money.Amount, to string, chainID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", asset, amount.String(), to, chainID)))
	return hex.EncodeToString(sum[:])
}

// normaliseAddress accepts any casing and stores lowercase, but refuses a
// mixed-case address whose EIP-55 checksum does not match: that casing is a
// checksum, and ignoring a wrong one would send money to an address the user
// mistyped.
func normaliseAddress(address string) (string, error) {
	if !common.IsHexAddress(address) {
		return "", fmt.Errorf("%w: %q is not an EVM address", ErrInvalid, address)
	}
	parsed := common.HexToAddress(address)
	body := strings.TrimPrefix(address, "0x")
	mixed := body != strings.ToLower(body) && body != strings.ToUpper(body)
	if mixed && parsed.Hex() != address {
		return "", fmt.Errorf("%w: %s fails its EIP-55 checksum", ErrInvalid, address)
	}
	if parsed == (common.Address{}) {
		return "", fmt.Errorf("%w: the zero address is not a withdrawal destination", ErrInvalid)
	}
	return strings.ToLower(parsed.Hex()), nil
}

// emit appends one withdrawal event to the outbox inside the caller's
// transaction (docs/plan-v1.0.md §7). previous is empty for the first event.
func emit(ctx context.Context, tx pgx.Tx, l *ledger.Service, tenant, eventType string, row sqlcgen.ChainWithdrawal, previous, reason string) error {
	seq, err := l.NextAccountSeq(ctx, tx, row.AccountID)
	if err != nil {
		return fmt.Errorf("withdrawal: account seq for %s: %w", row.AccountID, err)
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	txHash := deref(row.TxHash)
	if row.CancelTxHash != nil {
		txHash = *row.CancelTxHash
	}
	env, err := Event(eventType, tenant, Payload{
		WithdrawalID: row.ID, AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
		ToAddress: row.ToAddress, ChainID: row.ChainID, Status: row.Status,
		PreviousStatus: previous, Reason: reason, TxHash: txHash,
	}, seq, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return err
	}
	env.CorrelationID = deref(row.CorrelationID)
	_, err = eventbus.Outbox{}.Append(ctx, tx, env)
	return err
}

func records(rows []sqlcgen.ChainWithdrawal) ([]Record, error) {
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		rec, err := recordFrom(row)
		if err != nil {
			return nil, fmt.Errorf("withdrawal %s: %w", row.ID, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func inTx(ctx context.Context, db *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("withdrawal: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
