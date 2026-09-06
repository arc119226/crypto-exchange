package signer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// KeystoreSigner signs with keys derived from the HD seed the signer role
// holds in memory (docs/plan-v1.0.md §6.6). It is the v1 implementation;
// KMSSigner is the seam for a hardware or cloud key.
type KeystoreSigner struct {
	db      *pgxpool.Pool
	tenant  string
	chainID int64
	wallet  *hdwallet.Wallet
	reg     registry.Reader
	audit   *audit.Recorder
	log     *slog.Logger
	hot     common.Address
}

var _ Signer = (*KeystoreSigner)(nil)

// NewKeystoreSigner derives the hot wallet address once and keeps it.
func NewKeystoreSigner(db *pgxpool.Pool, tenant string, chainID int64, w *hdwallet.Wallet, reg registry.Reader, rec *audit.Recorder, log *slog.Logger) (*KeystoreSigner, error) {
	hot, err := w.Address(hdwallet.HotWalletPath())
	if err != nil {
		return nil, fmt.Errorf("signer: derive hot wallet: %w", err)
	}
	return &KeystoreSigner{db: db, tenant: tenant, chainID: chainID, wallet: w, reg: reg, audit: rec, log: log, hot: hot}, nil
}

// HotWallet implements Signer.
func (s *KeystoreSigner) HotWallet(context.Context) (common.Address, error) { return s.hot, nil }

// Sign implements Signer (docs/plan-v1.0.md §6.6).
//
// The order is the whole point: check the request against the database, build
// the transaction here, record the signature under a unique key, and only then
// hand the bytes back. The signing log INSERT is inside the same transaction
// as nothing else on purpose — it is committed *before* the signature leaves
// this process, so a crash between signing and replying leaves a record that
// blocks a second signature rather than an unrecorded transaction that does
// not.
func (s *KeystoreSigner) Sign(ctx context.Context, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	if req.ChainID != s.chainID {
		return Result{}, fmt.Errorf("%w: request is for chain %d, this signer serves %d", ErrRefused, req.ChainID, s.chainID)
	}

	var (
		to    common.Address
		value *big.Int
		data  []byte
		err   error
	)
	switch req.Kind {
	case KindWithdrawal:
		to, value, data, err = s.withdrawalTx(ctx, req)
	case KindNonceFill:
		to, value, data, err = s.nonceFillTx(req)
	case KindSweep, KindGasFund:
		return Result{}, fmt.Errorf("%w: %s arrives with sweeping in 4c", ErrUnsupported, req.Kind)
	default:
		return Result{}, fmt.Errorf("%w: kind %q", ErrRefused, req.Kind)
	}
	if err != nil {
		return Result{}, err
	}

	key, err := s.wallet.Derive(hdwallet.HotWalletPath())
	if err != nil {
		return Result{}, fmt.Errorf("signer: derive: %w", err)
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(s.chainID)), &types.DynamicFeeTx{
		ChainID: big.NewInt(s.chainID), Nonce: req.Nonce, To: &to, Value: value,
		Gas: req.Gas, GasTipCap: new(big.Int).Set(req.TipCap), GasFeeCap: new(big.Int).Set(req.FeeCap),
		Data: data,
	})
	if err != nil {
		return Result{}, fmt.Errorf("signer: sign %s %s: %w", req.Kind, req.RefID, err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return Result{}, fmt.Errorf("signer: encode signed tx: %w", err)
	}
	hash := strings.ToLower(tx.Hash().Hex())

	if existing, err := s.record(ctx, req, to, hash, raw); err != nil {
		return Result{}, err
	} else if existing != nil {
		// This intent was already signed. Return the transaction that exists
		// rather than the one just built: a caller that crashed between
		// signing and recording must be able to recover the transaction it
		// already caused to exist, and by now the fees it would ask for have
		// moved, so re-deriving would produce different bytes for the same
		// nonce -- two valid transactions racing for one slot.
		s.log.Warn("returning an already-signed transaction",
			slog.String("kind", string(req.Kind)), slog.String("ref_id", req.RefID),
			slog.Int("attempt", int(req.Attempt)), slog.String("tx_hash", existing.TxHash))
		return *existing, nil
	}
	s.log.Info("signed",
		slog.String("kind", string(req.Kind)), slog.String("ref_id", req.RefID),
		slog.Int("attempt", int(req.Attempt)), slog.Uint64("nonce", req.Nonce),
		slog.String("tx_hash", hash))
	return Result{RawTx: raw, TxHash: hash, From: strings.ToLower(s.hot.Hex()), Nonce: req.Nonce}, nil
}

// withdrawalTx checks the request against chain.withdrawals and returns what
// the transaction must actually contain.
//
// For a native withdrawal that is the recipient and the amount. For a token it
// is the *contract* and a transfer call, which is why the check cannot be done
// on a pre-built transaction without decoding calldata: the recipient the user
// asked for never appears in the transaction's `to` field at all.
func (s *KeystoreSigner) withdrawalTx(ctx context.Context, req Request) (common.Address, *big.Int, []byte, error) {
	row, err := sqlcgen.New(s.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: s.tenant, ID: req.RefID})
	if errors.Is(err, pgx.ErrNoRows) {
		return common.Address{}, nil, nil, fmt.Errorf("%w: no withdrawal %s", ErrRefused, req.RefID)
	}
	if err != nil {
		return common.Address{}, nil, nil, fmt.Errorf("signer: read withdrawal %s: %w", req.RefID, err)
	}
	// Only funds_locked may be signed for the first time. A replacement is
	// signed while the withdrawal is already broadcast, which is the one case
	// where a later state is legitimate — and it is only reachable with an
	// attempt above zero, which the unique key ties to a real replacement.
	switch {
	case req.Attempt == 0 && row.Status != "funds_locked":
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is %s, not funds_locked", ErrRefused, req.RefID, row.Status)
	case req.Attempt > 0 && row.Status != "broadcast":
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is %s, so there is nothing to replace", ErrRefused, req.RefID, row.Status)
	case req.Attempt > 0 && int32(req.Attempt) != row.Replacements+1:
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is on replacement %d, not %d", ErrRefused, req.RefID, row.Replacements, req.Attempt)
	case req.Attempt > 0 && (row.Nonce == nil || uint64(*row.Nonce) != req.Nonce): //nolint:gosec // CHECKed >= 0
		return common.Address{}, nil, nil, fmt.Errorf("%w: a replacement must reuse the original nonce", ErrRefused)
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return common.Address{}, nil, nil, fmt.Errorf("signer: amount of %s: %w", req.RefID, err)
	}
	// Everything the request claims must match the row. A mismatch is not a
	// disagreement to resolve — it means the caller is asking to send
	// something other than what a person approved.
	switch {
	case row.ChainID != req.ChainID:
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is on chain %d", ErrRefused, req.RefID, row.ChainID)
	case !strings.EqualFold(row.ToAddress, req.To.Hex()):
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s pays %s, not %s", ErrRefused, req.RefID, row.ToAddress, strings.ToLower(req.To.Hex()))
	case row.Asset != req.Asset:
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is %s, not %s", ErrRefused, req.RefID, row.Asset, req.Asset)
	case !amount.Equal(req.Value):
		return common.Address{}, nil, nil, fmt.Errorf("%w: withdrawal %s is for %s, not %s", ErrRefused, req.RefID, amount, req.Value)
	}

	asset, err := s.reg.GetAsset(ctx, s.tenant, row.Asset)
	if err != nil {
		return common.Address{}, nil, nil, fmt.Errorf("signer: asset %s: %w", row.Asset, err)
	}
	units, err := evm.ToWei(amount, asset.Scale)
	if err != nil {
		return common.Address{}, nil, nil, fmt.Errorf("%w: %s: %w", ErrRefused, req.RefID, err)
	}
	if asset.IsNative {
		return req.To, units, nil, nil
	}
	if asset.ContractAddress == nil || !common.IsHexAddress(*asset.ContractAddress) {
		return common.Address{}, nil, nil, fmt.Errorf("%w: %s has no contract address", ErrRefused, asset.Symbol)
	}
	data, err := evm.TransferCalldata(req.To, units)
	if err != nil {
		return common.Address{}, nil, nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	// A token transfer moves no ether: the value is zero and the recipient is
	// inside the calldata.
	return common.HexToAddress(*asset.ContractAddress), new(big.Int), data, nil
}

// nonceFillTx is the 0-value self-transfer that closes a nonce gap (§6.4.2).
// It can only ever pay the hot wallet, and can only ever pay it nothing.
func (s *KeystoreSigner) nonceFillTx(req Request) (common.Address, *big.Int, []byte, error) {
	if req.To != s.hot {
		return common.Address{}, nil, nil, fmt.Errorf("%w: a nonce fill may only pay the hot wallet", ErrRefused)
	}
	if !req.Value.IsZero() {
		return common.Address{}, nil, nil, fmt.Errorf("%w: a nonce fill moves nothing, got %s", ErrRefused, req.Value)
	}
	return s.hot, new(big.Int), nil, nil
}

// record appends to chain.signing_log and the audit trail, and reports whether
// this intent had already been signed.
//
// The unique key on (kind, ref_id, attempt) is what makes a second *distinct*
// signature impossible. A collision is not an error: it means the same intent
// was signed before, and the recorded transaction is returned so the caller
// converges on it. What the key prevents is two different transactions for one
// intent, which is the thing that would actually cost money.
func (s *KeystoreSigner) record(ctx context.Context, req Request, to common.Address, hash string, raw []byte) (*Result, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("signer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := sqlcgen.New(tx).InsertSignature(ctx, sqlcgen.InsertSignatureParams{
		TenantID: s.tenant, Kind: string(req.Kind), RefID: req.RefID, Attempt: req.Attempt,
		ChainID: s.chainID, FromAddress: strings.ToLower(s.hot.Hex()),
		ToAddress: strings.ToLower(to.Hex()), Nonce: int64(req.Nonce), //nolint:gosec // node nonces are far below 2^63
		TxHash: hash, RawTx: raw,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return s.existing(ctx, req)
		}
		return nil, fmt.Errorf("signer: record signature: %w", err)
	}
	if err := s.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "signer", Action: "signer.sign",
		TargetType: string(req.Kind), TargetID: req.RefID,
		After: map[string]any{
			"attempt": req.Attempt, "nonce": req.Nonce, "tx_hash": hash,
			"to": strings.ToLower(to.Hex()), "asset": req.Asset, "value": req.Value.String(),
		},
	}); err != nil {
		return nil, fmt.Errorf("signer: audit: %w", err)
	}
	return nil, tx.Commit(ctx)
}

// existing reads back a signature that was already made for this intent.
func (s *KeystoreSigner) existing(ctx context.Context, req Request) (*Result, error) {
	row, err := sqlcgen.New(s.db).GetSignature(ctx, sqlcgen.GetSignatureParams{
		TenantID: s.tenant, Kind: string(req.Kind), RefID: req.RefID, Attempt: req.Attempt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s attempt %d, and it could not be read back: %w",
			ErrAlreadySigned, req.Kind, req.RefID, req.Attempt, err)
	}
	if uint64(row.Nonce) != req.Nonce { //nolint:gosec // CHECKed >= 0
		// The same intent was signed for a different nonce. Returning either
		// transaction would be wrong, and signing a third is worse.
		return nil, fmt.Errorf("%w: %s %s attempt %d was signed with nonce %d, not %d",
			ErrAlreadySigned, req.Kind, req.RefID, req.Attempt, row.Nonce, req.Nonce)
	}
	return &Result{RawTx: row.RawTx, TxHash: row.TxHash, From: row.FromAddress, Nonce: req.Nonce}, nil
}
