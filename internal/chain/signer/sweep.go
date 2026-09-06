package signer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// sweepTx checks the request against chain.sweeps and returns the transfer
// that empties a deposit address into the hot wallet (docs/plan-v1.0.md
// §6.4.3).
//
// This is the only kind signed by a key other than the hot wallet's: the
// transaction is sent *by* the deposit address, so it is signed with that
// address's own derived key. Which key that is comes from the row, never from
// the request — the derivation index is what decides whose money moves, and a
// caller that could choose it could empty any address in the pool.
//
// The destination is not taken from the request either. A sweep can only ever
// pay the hot wallet; there is no legitimate sweep to anywhere else, so this
// is a constant rather than a check.
func (s *KeystoreSigner) sweepTx(ctx context.Context, req Request) (outgoing, error) {
	row, address, err := s.sweepAndAddress(ctx, req.RefID)
	if err != nil {
		return outgoing{}, err
	}
	asset, err := s.reg.GetAsset(ctx, s.tenant, row.Asset)
	if err != nil {
		return outgoing{}, fmt.Errorf("signer: asset %s: %w", row.Asset, err)
	}
	// Which state a sweep may be signed in depends on whether it needed gas
	// funding: a token sweep is only signable once the address has been given
	// enough ether to pay for the transfer, a native one from the start.
	// Signing a token sweep any earlier produces a transaction the sender
	// cannot afford.
	wanted := "requested"
	if !asset.IsNative {
		wanted = "gas_funded"
	}
	if row.Status != wanted {
		return outgoing{}, fmt.Errorf("%w: sweep %s is %s, and a %s sweep is signed at %s",
			ErrRefused, req.RefID, row.Status, row.Asset, wanted)
	}
	// Unlike a withdrawal there is no fee bump: if the transaction sits unmined
	// the balance is still there and the next tick starts a fresh sweep, so a
	// replacement would be a second claim on the same money.
	if req.Attempt != 0 {
		return outgoing{}, fmt.Errorf("%w: a sweep has only one attempt, got %d", ErrRefused, req.Attempt)
	}
	if req.To != s.hot {
		return outgoing{}, fmt.Errorf("%w: a sweep may only pay the hot wallet, not %s", ErrRefused, strings.ToLower(req.To.Hex()))
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return outgoing{}, fmt.Errorf("signer: amount of sweep %s: %w", req.RefID, err)
	}
	switch {
	case row.ChainID != req.ChainID:
		return outgoing{}, fmt.Errorf("%w: sweep %s is on chain %d", ErrRefused, req.RefID, row.ChainID)
	case row.Asset != req.Asset:
		return outgoing{}, fmt.Errorf("%w: sweep %s is %s, not %s", ErrRefused, req.RefID, row.Asset, req.Asset)
	case !amount.Equal(req.Value):
		return outgoing{}, fmt.Errorf("%w: sweep %s is for %s, not %s", ErrRefused, req.RefID, amount, req.Value)
	case row.Nonce == nil || uint64(*row.Nonce) != req.Nonce: //nolint:gosec // CHECKed >= 0
		// The nonce is pinned on the row before anything is signed, so that a
		// crash between signing and recording comes back asking for the same
		// transaction rather than a second one on a different nonce.
		return outgoing{}, fmt.Errorf("%w: sweep %s is pinned to another nonce", ErrRefused, req.RefID)
	}

	path, err := hdwallet.DepositPath(uint32(address.DerivationIndex)) //nolint:gosec // CHECKed >= 0 and bounded by MaxDepositIndex
	if err != nil {
		return outgoing{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	units, err := evm.ToWei(amount, asset.Scale)
	if err != nil {
		return outgoing{}, fmt.Errorf("%w: %s: %w", ErrRefused, req.RefID, err)
	}
	if asset.IsNative {
		return outgoing{path: path, to: s.hot, value: units}, nil
	}
	if asset.ContractAddress == nil || !common.IsHexAddress(*asset.ContractAddress) {
		return outgoing{}, fmt.Errorf("%w: %s has no contract address", ErrRefused, asset.Symbol)
	}
	data, err := evm.TransferCalldata(s.hot, units)
	if err != nil {
		return outgoing{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return outgoing{path: path, to: common.HexToAddress(*asset.ContractAddress), value: new(big.Int), data: data}, nil
}

// gasFundTx is the first leg of a token sweep: the hot wallet sends a deposit
// address exactly enough ether to pay for its own transfer (§6.4.3).
//
// An address that has only ever received tokens holds no ether, so it cannot
// pay for anything. This is the exception that makes the two-step shape
// necessary, and it is the one transaction in the system that deliberately
// sends value *to* a deposit address.
//
// The destination is read from the row and checked against the address pool.
// Signing a payment to an address this exchange does not control would be a
// transfer out of the hot wallet dressed as housekeeping.
func (s *KeystoreSigner) gasFundTx(ctx context.Context, req Request) (outgoing, error) {
	row, address, err := s.sweepAndAddress(ctx, req.RefID)
	if err != nil {
		return outgoing{}, err
	}
	if row.Status != "requested" {
		return outgoing{}, fmt.Errorf("%w: sweep %s is %s, which needs no gas funding", ErrRefused, req.RefID, row.Status)
	}
	if req.Attempt != 0 {
		return outgoing{}, fmt.Errorf("%w: gas funding has only one attempt, got %d", ErrRefused, req.Attempt)
	}
	if row.ChainID != req.ChainID {
		return outgoing{}, fmt.Errorf("%w: sweep %s is on chain %d", ErrRefused, req.RefID, row.ChainID)
	}
	// The recipient must be the address this sweep is emptying, and that
	// address must be one of ours. sweepAndAddress already proved the second
	// by finding the pool row; this proves the request agrees with it.
	to := common.HexToAddress(address.Address)
	if req.To != to {
		return outgoing{}, fmt.Errorf("%w: sweep %s empties %s, not %s", ErrRefused, req.RefID, address.Address, strings.ToLower(req.To.Hex()))
	}
	if !row.GasFundingAmount.Valid {
		return outgoing{}, fmt.Errorf("%w: sweep %s has no funding amount recorded", ErrRefused, req.RefID)
	}
	funding, err := pg.AmountFromNumeric(row.GasFundingAmount)
	if err != nil {
		return outgoing{}, fmt.Errorf("signer: funding amount of %s: %w", req.RefID, err)
	}
	if !funding.Equal(req.Value) {
		return outgoing{}, fmt.Errorf("%w: sweep %s funds %s, not %s", ErrRefused, req.RefID, funding, req.Value)
	}
	if row.GasFundingNonce == nil || uint64(*row.GasFundingNonce) != req.Nonce { //nolint:gosec // CHECKed >= 0
		return outgoing{}, fmt.Errorf("%w: sweep %s pinned another funding nonce", ErrRefused, req.RefID)
	}
	// Gas is the chain's own coin, so this leg is always a plain value
	// transfer at the native scale, whatever asset the sweep itself moves.
	units, err := evm.ToWei(funding, evm.MaxScale)
	if err != nil {
		return outgoing{}, fmt.Errorf("%w: %s: %w", ErrRefused, req.RefID, err)
	}
	return outgoing{path: hdwallet.HotWalletPath(), to: to, value: units}, nil
}

// sweepAndAddress reads the sweep and the pool row it names. A sweep whose
// address is not in the pool is not signable at any price: the derivation
// index is the only thing that says whose key may move that money.
func (s *KeystoreSigner) sweepAndAddress(ctx context.Context, id string) (sqlcgen.ChainSweep, sqlcgen.ChainDepositAddress, error) {
	q := sqlcgen.New(s.db)
	row, err := q.GetSweep(ctx, sqlcgen.GetSweepParams{TenantID: s.tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.ChainSweep{}, sqlcgen.ChainDepositAddress{}, fmt.Errorf("%w: no sweep %s", ErrRefused, id)
	}
	if err != nil {
		return sqlcgen.ChainSweep{}, sqlcgen.ChainDepositAddress{}, fmt.Errorf("signer: read sweep %s: %w", id, err)
	}
	address, err := q.GetDepositAddressByID(ctx, sqlcgen.GetDepositAddressByIDParams{TenantID: s.tenant, ID: row.AddressID})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.ChainSweep{}, sqlcgen.ChainDepositAddress{}, fmt.Errorf("%w: sweep %s names an address this exchange does not control", ErrRefused, id)
	}
	if err != nil {
		return sqlcgen.ChainSweep{}, sqlcgen.ChainDepositAddress{}, fmt.Errorf("signer: read address of %s: %w", id, err)
	}
	if !strings.EqualFold(address.Address, row.FromAddress) {
		return sqlcgen.ChainSweep{}, sqlcgen.ChainDepositAddress{}, fmt.Errorf("%w: sweep %s says %s, the pool says %s", ErrRefused, id, row.FromAddress, address.Address)
	}
	return row, address, nil
}
