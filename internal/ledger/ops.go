package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Ref identifies the business object an entry belongs to.
type Ref struct {
	Type string // order | trade | withdrawal | deposit | adjustment | sweep
	ID   string
}

// HoldParams moves available → hold for one account and asset.
type HoldParams struct {
	AccountID      string
	Asset          string
	Amount         money.Amount
	IdempotencyKey string // e.g. hold:order:{order_id}
	Ref            Ref
	CorrelationID  string
}

// Hold freezes funds (docs/plan-v1.0.md §6.1.3). Insufficient available
// funds return ErrInsufficient.
func (s *Service) Hold(ctx context.Context, tx pgx.Tx, p HoldParams) (JournalEntry, bool, error) {
	e, err := holdEntry(p)
	if err != nil {
		return JournalEntry{}, false, err
	}
	return s.Post(ctx, tx, e)
}

// BeginHold is Hold split into its round trips (see PendingEntry).
func (s *Service) BeginHold(p HoldParams) (*PendingEntry, error) {
	e, err := holdEntry(p)
	if err != nil {
		return nil, err
	}
	return s.Begin(e)
}

func holdEntry(p HoldParams) (Entry, error) {
	if !p.Amount.IsPositive() {
		return Entry{}, fmt.Errorf("%w: hold amount must be positive", ErrInvalidEntry)
	}
	return Entry{
		IdempotencyKey: p.IdempotencyKey, Kind: KindHold, RefType: p.Ref.Type, RefID: p.Ref.ID, CorrelationID: p.CorrelationID,
		Postings: []Posting{
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketAvailable, Direction: Debit, Amount: p.Amount},
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketHold, Direction: Credit, Amount: p.Amount},
		},
	}, nil
}

// Release moves hold → available (cancel, IOC remainder, failed withdrawal).
func (s *Service) Release(ctx context.Context, tx pgx.Tx, p HoldParams) (JournalEntry, bool, error) {
	e, err := releaseEntry(p)
	if err != nil {
		return JournalEntry{}, false, err
	}
	return s.Post(ctx, tx, e)
}

// BeginRelease is Release split into its round trips (see PendingEntry).
func (s *Service) BeginRelease(p HoldParams) (*PendingEntry, error) {
	e, err := releaseEntry(p)
	if err != nil {
		return nil, err
	}
	return s.Begin(e)
}

func releaseEntry(p HoldParams) (Entry, error) {
	if !p.Amount.IsPositive() {
		return Entry{}, fmt.Errorf("%w: release amount must be positive", ErrInvalidEntry)
	}
	return Entry{
		IdempotencyKey: p.IdempotencyKey, Kind: KindRelease, RefType: p.Ref.Type, RefID: p.Ref.ID, CorrelationID: p.CorrelationID,
		Postings: []Posting{
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketHold, Direction: Debit, Amount: p.Amount},
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketAvailable, Direction: Credit, Amount: p.Amount},
		},
	}, nil
}

// CreditParams books funds into a user's available balance from a house
// account (deposit: custody_deposit_addresses; faucet/adjustment: external).
type CreditParams struct {
	AccountID      string
	Asset          string
	Amount         money.Amount
	Source         HouseCode
	Kind           string // default credit
	IdempotencyKey string // e.g. deposit:{chain}:{tx}:{log}
	Ref            Ref
	Reason         string
	CorrelationID  string
}

// Credit books a deposit-like inflow: debit the source house account,
// credit the user's available bucket.
func (s *Service) Credit(ctx context.Context, tx pgx.Tx, p CreditParams) (JournalEntry, bool, error) {
	if !p.Amount.IsPositive() {
		return JournalEntry{}, false, fmt.Errorf("%w: credit amount must be positive", ErrInvalidEntry)
	}
	src, err := s.HouseAccount(p.Source)
	if err != nil {
		return JournalEntry{}, false, err
	}
	kind := p.Kind
	if kind == "" {
		kind = KindCredit
	}
	return s.Post(ctx, tx, Entry{
		IdempotencyKey: p.IdempotencyKey, Kind: kind, RefType: p.Ref.Type, RefID: p.Ref.ID, Reason: p.Reason, CorrelationID: p.CorrelationID,
		Postings: []Posting{
			{AccountID: src, Asset: p.Asset, Bucket: BucketHouse, Direction: Debit, Amount: p.Amount},
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketAvailable, Direction: Credit, Amount: p.Amount},
		},
	})
}

// AdjustParams is a manual correction by an administrator against the
// external account (docs/plan-v1.0.md §6.1.4 g). Direction credit adds to
// the user's available balance (dev faucet), debit removes.
type AdjustParams struct {
	AccountID      string
	Asset          string
	Amount         money.Amount
	Direction      Direction
	Reason         string
	IdempotencyKey string // e.g. adjust:{adjustment_id}
	CorrelationID  string
}

// Adjust posts an administrator adjustment. Reason is mandatory; the
// caller records the audit event in the same transaction.
func (s *Service) Adjust(ctx context.Context, tx pgx.Tx, p AdjustParams) (JournalEntry, bool, error) {
	if p.Reason == "" {
		return JournalEntry{}, false, ErrReasonRequired
	}
	if !p.Amount.IsPositive() {
		return JournalEntry{}, false, fmt.Errorf("%w: adjustment amount must be positive", ErrInvalidEntry)
	}
	ext, err := s.HouseAccount(HouseExternal)
	if err != nil {
		return JournalEntry{}, false, err
	}
	var postings []Posting
	switch p.Direction {
	case Credit:
		postings = []Posting{
			{AccountID: ext, Asset: p.Asset, Bucket: BucketHouse, Direction: Debit, Amount: p.Amount},
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketAvailable, Direction: Credit, Amount: p.Amount},
		}
	case Debit:
		postings = []Posting{
			{AccountID: p.AccountID, Asset: p.Asset, Bucket: BucketAvailable, Direction: Debit, Amount: p.Amount},
			{AccountID: ext, Asset: p.Asset, Bucket: BucketHouse, Direction: Credit, Amount: p.Amount},
		}
	default:
		return JournalEntry{}, false, fmt.Errorf("%w: direction %q", ErrInvalidEntry, p.Direction)
	}
	return s.Post(ctx, tx, Entry{
		IdempotencyKey: p.IdempotencyKey, Kind: KindAdjustment, RefType: "adjustment", RefID: p.IdempotencyKey,
		Reason: p.Reason, CorrelationID: p.CorrelationID, Postings: postings,
	})
}

// HouseAdjustParams books value into or out of a custody account against
// external (docs/plan-v1.0.md §6.1.4 g).
//
// This is how money that entered or left the exchange's control without a
// transaction the ledger produced gets recorded: a faucet funding the hot
// wallet, an operator moving coins in from cold storage, or the resolution of
// a reconciliation break whose cause has been established. Direction credit
// means custody gains; debit means it loses.
type HouseAdjustParams struct {
	Code           HouseCode
	Asset          string
	Amount         money.Amount
	Direction      Direction
	Reason         string
	IdempotencyKey string
	CorrelationID  string
}

// AdjustHouse posts a custody adjustment. Reason is mandatory; the caller
// records the audit event in the same transaction.
//
// Only the two custody accounts are allowed. They are the ones that can
// legitimately gain or lose value outside this ledger, because they are the
// ones backed by an address somebody else can send to. fee_revenue,
// gas_expense and pending_withdrawal are derived from entries this system
// makes, so an adjustment to one of them would not be recording a fact -- it
// would be hiding a bug.
func (s *Service) AdjustHouse(ctx context.Context, tx pgx.Tx, p HouseAdjustParams) (JournalEntry, bool, error) {
	if p.Code != HouseCustodyHot && p.Code != HouseCustodyDepositAddresses {
		return JournalEntry{}, false, fmt.Errorf("%w: %s is not a custody account", ErrInvalidEntry, p.Code)
	}
	if p.Reason == "" {
		return JournalEntry{}, false, ErrReasonRequired
	}
	if !p.Amount.IsPositive() {
		return JournalEntry{}, false, fmt.Errorf("%w: adjustment amount must be positive", ErrInvalidEntry)
	}
	custody, err := s.HouseAccount(p.Code)
	if err != nil {
		return JournalEntry{}, false, err
	}
	ext, err := s.HouseAccount(HouseExternal)
	if err != nil {
		return JournalEntry{}, false, err
	}
	var postings []Posting
	switch p.Direction {
	case Credit:
		postings = []Posting{
			{AccountID: custody, Asset: p.Asset, Bucket: BucketHouse, Direction: Debit, Amount: p.Amount},
			{AccountID: ext, Asset: p.Asset, Bucket: BucketHouse, Direction: Credit, Amount: p.Amount},
		}
	case Debit:
		postings = []Posting{
			{AccountID: ext, Asset: p.Asset, Bucket: BucketHouse, Direction: Debit, Amount: p.Amount},
			{AccountID: custody, Asset: p.Asset, Bucket: BucketHouse, Direction: Credit, Amount: p.Amount},
		}
	default:
		return JournalEntry{}, false, fmt.Errorf("%w: direction %q", ErrInvalidEntry, p.Direction)
	}
	return s.Post(ctx, tx, Entry{
		IdempotencyKey: p.IdempotencyKey, Kind: KindAdjustment, RefType: "house_adjustment", RefID: p.IdempotencyKey,
		Reason: p.Reason, CorrelationID: p.CorrelationID, Postings: postings,
	})
}

// Settle books one trade: both holds pay the counterparties net of fees,
// fees go to fee_revenue, and the buyer's price improvement is released.
func (s *Service) Settle(ctx context.Context, tx pgx.Tx, p SettleParams) (SettleResult, bool, error) {
	feeRevenue, err := s.HouseAccount(HouseFeeRevenue)
	if err != nil {
		return SettleResult{}, false, err
	}
	entry, res, err := BuildSettleEntry(p, feeRevenue)
	if err != nil {
		return SettleResult{}, false, err
	}
	je, replayed, err := s.Post(ctx, tx, entry)
	if err != nil {
		return SettleResult{}, false, err
	}
	res.Entry = je
	return res, replayed, nil
}

// BeginSettle is Settle split into its round trips (see PendingEntry). The
// fees and the release are computed here, before anything is sent; the
// result's Entry is what PendingEntry.Entry returns after Finish.
func (s *Service) BeginSettle(p SettleParams) (*PendingEntry, SettleResult, error) {
	feeRevenue, err := s.HouseAccount(HouseFeeRevenue)
	if err != nil {
		return nil, SettleResult{}, err
	}
	entry, res, err := BuildSettleEntry(p, feeRevenue)
	if err != nil {
		return nil, SettleResult{}, err
	}
	pe, err := s.Begin(entry)
	if err != nil {
		return nil, SettleResult{}, err
	}
	return pe, res, nil
}
