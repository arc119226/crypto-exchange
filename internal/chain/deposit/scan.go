package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// sighting is one incoming transfer found in a block.
type sighting struct {
	txHash      string
	logIndex    int32
	address     watchedAddress
	asset       registry.Asset
	amount      money.Amount
	blockNumber uint64
	blockHash   string
}

// scanRange records every deposit in [from, to] and advances the cursor.
func (s *Scanner) scanRange(ctx context.Context, from, to uint64) error {
	assets, err := s.assets(ctx)
	if err != nil {
		return err
	}
	// One log query for the whole range: an ERC-20 filter is by contract and
	// topic only, and reverted transactions' logs never appear, so a receipt
	// check is unnecessary on this path.
	logs, err := s.chain.TransferLogs(ctx, from, to, assets.contracts())
	if err != nil {
		return err
	}
	byBlock := map[uint64][]sighting{}
	for _, l := range logs {
		found, ok, err := s.erc20Sighting(l, assets)
		if err != nil {
			// A token we watch emitted something we cannot read. Skipping it
			// silently would lose a deposit, so say so and keep scanning.
			s.log.Error("unreadable transfer log",
				slog.String("tx", l.TxHash.Hex()), slog.Uint64("log_index", uint64(l.Index)),
				slog.String("err", err.Error()))
			s.metrics.unreadable.Inc()
			continue
		}
		if ok {
			byBlock[l.BlockNumber] = append(byBlock[l.BlockNumber], found)
		}
	}

	for n := from; n <= to; n++ {
		block, err := s.chain.BlockByNumber(ctx, n)
		if errors.Is(err, evm.ErrNotFound) {
			return nil // the head moved; resume next tick
		}
		if err != nil {
			return err
		}
		native, err := s.nativeSightings(ctx, block, assets)
		if err != nil {
			return err
		}
		found := append(native, byBlock[n]...)
		if err := s.commitBlock(ctx, block, found); err != nil {
			return err
		}
	}
	return nil
}

// nativeSightings finds plain value transfers into watched addresses.
func (s *Scanner) nativeSightings(ctx context.Context, block evm.Block, assets assetIndex) ([]sighting, error) {
	if assets.native.Symbol == "" {
		return nil, nil
	}
	var out []sighting
	for _, tx := range block.Txs {
		to := tx.To()
		if to == nil || tx.Value().Sign() <= 0 {
			continue
		}
		watched, ok := s.watched[*to]
		if !ok {
			continue
		}
		// A deposit is money arriving from outside the exchange. This one may
		// have come from inside it: the sweeper funds a deposit address with
		// ether from the hot wallet so the address can pay for its own token
		// transfer (§6.4.3), and that is a plain value transfer into a watched
		// address -- exactly the shape looked for here.
		//
		// Crediting it would hand the account free ether the exchange fronted,
		// and would book custody_deposit_addresses twice for one movement:
		// once by the sweeper moving it out of custody_hot, once by this
		// scanner treating it as an arrival. Reconciliation is what found it.
		if from, ok := s.senderOf(tx); ok && s.isOurs(from) {
			s.log.Info("ignoring a transfer the exchange sent itself",
				slog.String("tx", tx.Hash().Hex()), slog.String("to", watched.address))
			continue
		}
		// Unlike a log, a transaction appears in its block whether or not it
		// succeeded: an out-of-gas transfer moves nothing. Credit only what
		// actually landed.
		receipt, err := s.chain.Receipt(ctx, tx.Hash().Hex())
		if err != nil {
			return nil, err
		}
		if receipt.Status != 1 {
			s.log.Info("ignoring a failed transfer",
				slog.String("tx", tx.Hash().Hex()), slog.String("to", watched.address))
			continue
		}
		amount, err := evm.FromWei(tx.Value(), assets.native.Scale)
		if err != nil {
			s.log.Error("native transfer is not representable",
				slog.String("tx", tx.Hash().Hex()), slog.String("err", err.Error()))
			s.metrics.unreadable.Inc()
			continue
		}
		out = append(out, sighting{
			txHash: strings.ToLower(tx.Hash().Hex()), logIndex: NativeLogIndex,
			address: watched, asset: assets.native, amount: amount,
			blockNumber: block.Number, blockHash: block.Hash,
		})
	}
	return out, nil
}

// erc20Sighting turns a Transfer log into a sighting when its recipient is
// one of ours.
func (s *Scanner) erc20Sighting(l types.Log, assets assetIndex) (sighting, bool, error) {
	transfer, err := evm.DecodeTransfer(l)
	if err != nil {
		return sighting{}, false, err
	}
	watched, ok := s.watched[transfer.To]
	if !ok {
		return sighting{}, false, nil
	}
	// Same rule as the native path: what the exchange sends itself is not a
	// deposit. No token moves this way today -- gas funding is native and a
	// sweep goes to the hot wallet, which is not watched -- but the rule is
	// about where money came from, not about which paths happen to exist.
	if s.isOurs(transfer.From) {
		return sighting{}, false, nil
	}
	asset, ok := assets.byContract[transfer.Contract]
	if !ok {
		return sighting{}, false, nil // a token we no longer watch
	}
	if transfer.Value.Sign() <= 0 {
		return sighting{}, false, nil
	}
	amount, err := evm.FromWei(transfer.Value, asset.Scale)
	if err != nil {
		return sighting{}, false, err
	}
	return sighting{
		txHash: strings.ToLower(l.TxHash.Hex()), logIndex: int32(l.Index), //nolint:gosec // a log index fits int32
		address: watched, asset: asset, amount: amount,
		blockNumber: l.BlockNumber, blockHash: strings.ToLower(l.BlockHash.Hex()),
	}, true, nil
}

// commitBlock records a block and its deposits in one transaction, so the
// cursor never claims to have scanned a block whose deposits were not written.
func (s *Scanner) commitBlock(ctx context.Context, block evm.Block, found []sighting) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.UpsertBlock(ctx, sqlcgen.UpsertBlockParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
			Number: int64(block.Number), Hash: block.Hash, ParentHash: block.ParentHash, //nolint:gosec // bounded by the head
		}); err != nil {
			return fmt.Errorf("deposit: record block %d: %w", block.Number, err)
		}
		for _, f := range found {
			if err := s.record(ctx, tx, f); err != nil {
				return err
			}
		}
		return q.UpsertScanCursor(ctx, sqlcgen.UpsertScanCursorParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
			LastScannedBlock: int64(block.Number), LastBlockHash: block.Hash, //nolint:gosec // bounded by the head
		})
	})
}

// record writes a sighting, updating an existing row rather than inserting.
//
// This is the trap docs/plan-v1.0.md §6.4.1 calls out: after a reorg the same
// transaction reappears on the new chain, and an INSERT would hit the unique
// key, look like a duplicate, and leave the deposit uncreditable forever.
func (s *Scanner) record(ctx context.Context, tx pgx.Tx, f sighting) error {
	q := sqlcgen.New(tx)
	existing, err := q.GetDepositForUpdate(ctx, sqlcgen.GetDepositForUpdateParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, TxHash: f.txHash, LogIndex: f.logIndex,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row, err := q.InsertDeposit(ctx, sqlcgen.InsertDepositParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, TxHash: f.txHash, LogIndex: f.logIndex,
			Address: f.address.address, AccountID: f.address.accountID, Asset: f.asset.Symbol,
			Amount:      pg.NumericFromAmount(f.amount),
			BlockNumber: int64(f.blockNumber), BlockHash: f.blockHash, //nolint:gosec // bounded by the head
			Confirmations: 0, Status: StatusDetected,
			CorrelationID: ptrString(telemetry.CorrelationID(ctx)),
		})
		if err != nil {
			return fmt.Errorf("deposit: insert %s/%d: %w", f.txHash, f.logIndex, err)
		}
		s.log.Info("deposit detected",
			slog.String("tx", f.txHash), slog.String("asset", f.asset.Symbol),
			slog.String("amount", f.amount.String()), slog.Uint64("block", f.blockNumber))
		return s.emit(ctx, tx, EventDetected, row)
	case err != nil:
		return fmt.Errorf("deposit: lock %s/%d: %w", f.txHash, f.logIndex, err)
	case existing.Status == StatusCredited:
		return nil // already money; a later sighting changes nothing
	}

	reappeared := existing.Status == StatusOrphaned || existing.Status == StatusDropped
	row, err := q.UpdateDepositSighting(ctx, sqlcgen.UpdateDepositSightingParams{
		ID: existing.ID, TenantID: s.cfg.Tenant,
		BlockNumber: int64(f.blockNumber), BlockHash: f.blockHash, //nolint:gosec // bounded by the head
		Confirmations: 0, Status: StatusDetected,
	})
	if err != nil {
		return fmt.Errorf("deposit: update %s/%d: %w", f.txHash, f.logIndex, err)
	}
	if reappeared {
		s.log.Info("deposit reappeared on the canonical chain",
			slog.String("tx", f.txHash), slog.Uint64("block", f.blockNumber))
		return s.emit(ctx, tx, EventDetected, row)
	}
	return nil
}

// advance moves maturing deposits forward and credits the ones that are ready.
func (s *Scanner) advance(ctx context.Context, head uint64) error {
	rows, err := sqlcgen.New(s.db).ListMaturingDeposits(ctx, sqlcgen.ListMaturingDepositsParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
	})
	if err != nil {
		return fmt.Errorf("deposit: list maturing: %w", err)
	}
	assets, err := s.assets(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		block := uint64(row.BlockNumber) //nolint:gosec // the column is CHECKed >= 0
		if block > head {
			continue // the head moved backwards; the next tick sorts it out
		}
		// docs/domain.md §4: a deposit in the head block already has one
		// confirmation, so anvil's required_confirmations = 1 credits in the
		// same tick that detects it.
		confirmations := int32(min(head-block+1, uint64(int32Max))) //nolint:gosec // clamped
		required := assets.required(row.Asset, s.cfg.DefaultConfirmations)
		if confirmations >= required {
			if err := s.credit(ctx, row, confirmations, required); err != nil {
				return err
			}
			continue
		}
		if _, err := sqlcgen.New(s.db).UpdateDepositSighting(ctx, sqlcgen.UpdateDepositSightingParams{
			ID: row.ID, TenantID: s.cfg.Tenant, BlockNumber: row.BlockNumber, BlockHash: row.BlockHash,
			Confirmations: confirmations, Status: StatusConfirming,
		}); err != nil {
			return fmt.Errorf("deposit: confirm %s: %w", row.ID, err)
		}
	}
	return nil
}

const int32Max = 1<<31 - 1

// credit posts the deposit to the ledger and marks it credited, in one
// transaction with its event (ADR-0002). The idempotency key means a replay
// after a crash finds the entry already posted and writes nothing.
func (s *Scanner) credit(ctx context.Context, row sqlcgen.ChainDeposit, confirmations, required int32) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("deposit: amount of %s: %w", row.ID, err)
	}
	fee, err := s.depositFee(ctx, row, amount)
	if err != nil {
		return err
	}
	custody, err := s.ledger.HouseAccount(ledger.HouseCustodyDepositAddresses)
	if err != nil {
		return err
	}
	feeRevenue, err := s.ledger.HouseAccount(ledger.HouseFeeRevenue)
	if err != nil {
		return err
	}
	// The chain delivered amount; the user receives what is left after the
	// fee (§6.1.4 i). One entry with three postings rather than two entries,
	// because there is one event here -- money arrived -- and splitting it
	// would let a reader see a deposit credited without its fee.
	//
	// At the seeded rate of zero this is exactly the two postings
	// ledger.Credit would have made, which is what keeps every existing
	// deposit test unchanged.
	credit := amount.Sub(fee)
	postings := []ledger.Posting{
		{AccountID: custody, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amount},
		{AccountID: row.AccountID, Asset: row.Asset, Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: credit},
	}
	if fee.IsPositive() {
		postings = append(postings,
			ledger.Posting{AccountID: feeRevenue, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: fee})
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := s.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: fmt.Sprintf("deposit:%d:%s:%d", row.ChainID, row.TxHash, row.LogIndex),
			Kind:           "deposit",
			RefType:        "deposit", RefID: row.ID, Reason: "on-chain deposit",
			CorrelationID: deref(row.CorrelationID),
			Postings:      postings,
		}); err != nil {
			return fmt.Errorf("deposit: credit %s: %w", row.ID, err)
		}
		credited, err := sqlcgen.New(tx).MarkDepositCredited(ctx, sqlcgen.MarkDepositCreditedParams{
			ID: row.ID, TenantID: s.cfg.Tenant, Confirmations: confirmations,
			Fee: pg.NumericFromAmount(fee), CreditedAmount: pg.NumericFromAmount(credit),
		})
		if err != nil {
			return fmt.Errorf("deposit: mark credited %s: %w", row.ID, err)
		}
		s.log.Info("deposit credited",
			slog.String("deposit_id", row.ID), slog.String("account_id", row.AccountID),
			slog.String("asset", row.Asset), slog.String("amount", amount.String()),
			slog.String("fee", fee.String()), slog.String("credited", credit.String()),
			slog.Int("confirmations", int(confirmations)), slog.Int("required", int(required)))
		s.metrics.credited.WithLabelValues(row.Asset).Inc()
		return s.emit(ctx, tx, EventCredited, credited)
	})
}

// depositFee is what this deposit is charged (§23.4). It is zero for every
// asset the seed ships, so the common path is a registry read and an
// immediate zero.
//
// A deposit small enough that the fee would round up to the whole amount is
// credited in full instead. The alternative is worse in both directions:
// crediting zero is confiscation, and refusing to credit strands a user's
// money in a deposit the scanner would retry forever. The amounts involved
// are one unit of the asset -- the situation only arises from rounding -- so
// the exchange forgoes a rounding error and says so in the log.
func (s *Scanner) depositFee(ctx context.Context, row sqlcgen.ChainDeposit, amount money.Amount) (money.Amount, error) {
	asset, err := s.registry.GetAsset(ctx, s.cfg.Tenant, row.Asset)
	if err != nil {
		return money.Zero, fmt.Errorf("deposit: asset %s of %s: %w", row.Asset, row.ID, err)
	}
	if asset.DepositFeeBps == 0 {
		return money.Zero, nil
	}
	fee, err := asset.DepositFeeFor(amount)
	if err != nil {
		s.log.Warn("deposit is too small to charge a fee on; crediting it in full",
			slog.String("deposit_id", row.ID), slog.String("asset", row.Asset),
			slog.String("amount", amount.String()), slog.String("err", err.Error()))
		return money.Zero, nil
	}
	return fee, nil
}

// expireOrphans drops deposits that never came back (§6.4.1).
func (s *Scanner) expireOrphans(ctx context.Context, head uint64) error {
	cutoff := int64(0)
	if head > s.cfg.OrphanExpiryBlocks {
		cutoff = int64(head - s.cfg.OrphanExpiryBlocks) //nolint:gosec // bounded by the head
	}
	rows, err := sqlcgen.New(s.db).ListExpiredOrphans(ctx, sqlcgen.ListExpiredOrphansParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, OrphanedAtBlock: &cutoff,
	})
	if err != nil {
		return fmt.Errorf("deposit: list expired orphans: %w", err)
	}
	for _, row := range rows {
		if err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := sqlcgen.New(tx).MarkDepositDropped(ctx, sqlcgen.MarkDepositDroppedParams{
				ID: row.ID, TenantID: s.cfg.Tenant,
			}); err != nil {
				return fmt.Errorf("deposit: drop %s: %w", row.ID, err)
			}
			row.Status = StatusDropped
			return s.emit(ctx, tx, EventDropped, row)
		}); err != nil {
			return err
		}
		s.log.Warn("deposit dropped after never reappearing",
			slog.String("deposit_id", row.ID), slog.String("tx", row.TxHash))
	}
	return nil
}

// pruneRing keeps chain.blocks bounded.
func (s *Scanner) pruneRing(ctx context.Context, head uint64) error {
	if head <= s.cfg.RingDepth {
		return nil
	}
	if _, err := sqlcgen.New(s.db).PruneBlocksBelow(ctx, sqlcgen.PruneBlocksBelowParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
		Number: int64(head - s.cfg.RingDepth), //nolint:gosec // bounded by the head
	}); err != nil {
		return fmt.Errorf("deposit: prune blocks: %w", err)
	}
	return nil
}

// emit appends a deposit event, stamping the account's next sequence so a
// private-stream client can spot a gap.
func (s *Scanner) emit(ctx context.Context, tx pgx.Tx, eventType string, row sqlcgen.ChainDeposit) error {
	seq, err := s.ledger.NextAccountSeq(ctx, tx, row.AccountID)
	if err != nil {
		return fmt.Errorf("deposit: account seq for %s: %w", row.AccountID, err)
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	fee, err := pg.NullableAmountFromNumeric(row.Fee)
	if err != nil {
		return err
	}
	credited, err := pg.NullableAmountFromNumeric(row.CreditedAmount)
	if err != nil {
		return err
	}
	env, err := Event(eventType, s.cfg.Tenant, Payload{
		DepositID: row.ID, AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
		Address: row.Address, TxHash: row.TxHash, LogIndex: row.LogIndex,
		BlockNumber: uint64(row.BlockNumber), BlockHash: row.BlockHash, //nolint:gosec // CHECKed >= 0
		Confirmations: row.Confirmations, Status: row.Status,
		Fee: fee, Credited: credited,
	}, seq, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return err
	}
	env.CorrelationID = deref(row.CorrelationID)
	_, err = eventbus.Outbox{}.Append(ctx, tx, env)
	return err
}

func (s *Scanner) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("deposit: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// refreshAddresses adds addresses handed out since the last call. The set only
// grows, so resuming from the highest id already seen is enough.
func (s *Scanner) refreshAddresses(ctx context.Context) error {
	rows, err := sqlcgen.New(s.db).ListDepositAddresses(ctx, sqlcgen.ListDepositAddressesParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
	})
	if err != nil {
		return fmt.Errorf("deposit: load addresses: %w", err)
	}
	for _, r := range rows {
		if r.AccountID == nil {
			continue // still free in the pool; nobody can deposit to it yet
		}
		s.watched[common.HexToAddress(r.Address)] = watchedAddress{
			id: r.ID, accountID: *r.AccountID, address: r.Address,
		}
		if r.ID > s.watermark {
			s.watermark = r.ID
		}
	}
	s.metrics.observeWatched(len(s.watched))
	return s.refreshHotWallet(ctx)
}

// refreshHotWallet notes the address the exchange pays out of, so a transfer
// from it is not mistaken for a deposit.
//
// Absent until the signer has started once, which is fine: with no hot wallet
// there is nothing that could have sent such a transfer.
func (s *Scanner) refreshHotWallet(ctx context.Context) error {
	row, err := sqlcgen.New(s.db).GetHotWallet(ctx, sqlcgen.GetHotWalletParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deposit: load hot wallet: %w", err)
	}
	hot := common.HexToAddress(row.Address)
	s.hot = &hot
	return nil
}

// isOurs reports whether an address is one the exchange sends from: the hot
// wallet, or a deposit address it controls.
func (s *Scanner) isOurs(a common.Address) bool {
	if s.hot != nil && *s.hot == a {
		return true
	}
	_, ok := s.watched[a]
	return ok
}

// senderOf recovers who signed a transaction. A signature that will not
// recover is not something to guess about, so the caller treats it as
// "not ours" and the transfer is judged on its recipient alone.
func (s *Scanner) senderOf(tx *types.Transaction) (common.Address, bool) {
	from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(s.cfg.ChainID)), tx)
	if err != nil {
		return common.Address{}, false
	}
	return from, true
}

// assetIndex resolves a chain address or symbol to a registry asset.
type assetIndex struct {
	native     registry.Asset
	byContract map[common.Address]registry.Asset
	bySymbol   map[string]registry.Asset
}

func (a assetIndex) contracts() []common.Address {
	out := make([]common.Address, 0, len(a.byContract))
	for addr := range a.byContract {
		out = append(out, addr)
	}
	return out
}

func (a assetIndex) required(symbol string, fallback int32) int32 {
	if asset, ok := a.bySymbol[symbol]; ok && asset.RequiredConfirmations > 0 {
		return asset.RequiredConfirmations
	}
	return fallback
}

// assets rebuilds the index each tick: it is a handful of rows, and picking up
// a newly listed token should not need a restart.
func (s *Scanner) assets(ctx context.Context) (assetIndex, error) {
	rows, err := s.registry.ListAssets(ctx, s.cfg.Tenant)
	if err != nil {
		return assetIndex{}, fmt.Errorf("deposit: list assets: %w", err)
	}
	idx := assetIndex{byContract: map[common.Address]registry.Asset{}, bySymbol: map[string]registry.Asset{}}
	for _, a := range rows {
		if a.ChainID != s.cfg.ChainID || a.Status != registry.AssetActive || !a.DepositEnabled {
			continue
		}
		idx.bySymbol[a.Symbol] = a
		switch {
		case a.IsNative:
			idx.native = a
		case a.ContractAddress != nil:
			idx.byContract[common.HexToAddress(*a.ContractAddress)] = a
		}
	}
	return idx, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
