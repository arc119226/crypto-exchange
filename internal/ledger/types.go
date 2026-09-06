package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Bucket is the sub-account a posting hits. Spot accounts use available and
// hold; house accounts use house.
type Bucket string

// Buckets.
const (
	BucketAvailable Bucket = "available"
	BucketHold      Bucket = "hold"
	BucketHouse     Bucket = "house"
)

// Direction of a posting.
type Direction string

// Directions.
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// AccountKind distinguishes user spot accounts from the exchange's house
// accounts.
type AccountKind string

// Account kinds.
const (
	KindSpot  AccountKind = "spot"
	KindHouse AccountKind = "house"
)

// AccountStatus gates policy checks (frozen accounts cannot trade or
// withdraw; the ledger itself still posts, e.g. deposits and settlements of
// resting orders).
type AccountStatus string

// Account statuses.
const (
	StatusActive AccountStatus = "active"
	StatusFrozen AccountStatus = "frozen"
)

// HouseCode identifies one of the exchange's own accounts (asset lives on
// the posting).
type HouseCode string

// House account codes (docs/plan-v1.0.md §6.1.1).
const (
	HouseFeeRevenue              HouseCode = "fee_revenue"
	HouseGasExpense              HouseCode = "gas_expense"
	HouseCustodyHot              HouseCode = "custody_hot"
	HouseCustodyDepositAddresses HouseCode = "custody_deposit_addresses"
	HousePendingWithdrawal       HouseCode = "pending_withdrawal"
	HouseExternal                HouseCode = "external"
)

// AllHouseCodes lists every house account a tenant must have.
var AllHouseCodes = []HouseCode{
	HouseFeeRevenue, HouseGasExpense, HouseCustodyHot, HouseCustodyDepositAddresses, HousePendingWithdrawal, HouseExternal,
}

// AccountType is the accounting nature of an account: it decides which
// direction increases the balance.
type AccountType string

// Account types and their normal balance.
const (
	TypeLiability AccountType = "liability" // credit-normal: user balances, pending withdrawals
	TypeAsset     AccountType = "asset"     // debit-normal: custody
	TypeRevenue   AccountType = "revenue"   // credit-normal
	TypeExpense   AccountType = "expense"   // debit-normal
	TypeExternal  AccountType = "external"  // credit − debit by convention (docs/domain.md §1.1)
)

// Type returns the accounting type of a house account.
func (c HouseCode) Type() AccountType {
	switch c {
	case HouseFeeRevenue:
		return TypeRevenue
	case HouseGasExpense:
		return TypeExpense
	case HouseCustodyHot, HouseCustodyDepositAddresses:
		return TypeAsset
	case HousePendingWithdrawal:
		return TypeLiability
	default:
		return TypeExternal
	}
}

// Valid reports whether the code is one of AllHouseCodes.
func (c HouseCode) Valid() bool {
	for _, k := range AllHouseCodes {
		if k == c {
			return true
		}
	}
	return false
}

// DebitNormal reports whether debits increase balances of this type.
func (t AccountType) DebitNormal() bool { return t == TypeAsset || t == TypeExpense }

// Entry kinds recorded on journal_entries.kind.
const (
	KindHold       = "hold"
	KindRelease    = "release"
	KindSettle     = "settle"
	KindCredit     = "credit"
	KindAdjustment = "adjustment"
)

// Errors. ErrInsufficient and ErrUnbalanced are business outcomes the caller
// turns into rejections; the rest are caller bugs or missing setup.
var (
	ErrInvalidEntry      = errors.New("ledger: invalid entry")
	ErrUnbalanced        = errors.New("ledger: entry does not balance")
	ErrInsufficient      = errors.New("ledger: insufficient balance")
	ErrAccountNotFound   = errors.New("ledger: account not found")
	ErrAccountKind       = errors.New("ledger: posting bucket does not match account kind")
	ErrHouseAccount      = errors.New("ledger: house account missing")
	ErrNotFound          = errors.New("ledger: not found")
	ErrReasonRequired    = errors.New("ledger: adjustments require a reason")
	ErrInvalidSettlement = errors.New("ledger: invalid settlement")
)

// Account is a ledger account.
type Account struct {
	ID          string
	TenantID    string
	Kind        AccountKind
	HouseCode   HouseCode // empty for spot accounts
	OwnerUserID *string
	Status      AccountStatus
	NextSeq     int64
	Version     int32
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Posting is one line of a journal entry.
type Posting struct {
	AccountID string
	Asset     string
	Bucket    Bucket
	Direction Direction
	Amount    money.Amount
}

// Entry is the input to Post.
type Entry struct {
	IdempotencyKey string
	Kind           string
	RefType        string
	RefID          string
	Reason         string
	CorrelationID  string
	Postings       []Posting
}

// Validate checks the entry statically: a key, a kind, at least two
// postings, positive amounts, known buckets/directions, and per-asset
// Σdebit = Σcredit.
func (e Entry) Validate() error {
	if e.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency key required", ErrInvalidEntry)
	}
	if e.Kind == "" {
		return fmt.Errorf("%w: kind required", ErrInvalidEntry)
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("%w: at least two postings", ErrInvalidEntry)
	}
	sums := map[string]money.Amount{}
	order := []string{}
	for i, p := range e.Postings {
		if p.AccountID == "" || p.Asset == "" {
			return fmt.Errorf("%w: posting %d missing account or asset", ErrInvalidEntry, i)
		}
		if p.Bucket != BucketAvailable && p.Bucket != BucketHold && p.Bucket != BucketHouse {
			return fmt.Errorf("%w: posting %d bucket %q", ErrInvalidEntry, i, p.Bucket)
		}
		if p.Direction != Debit && p.Direction != Credit {
			return fmt.Errorf("%w: posting %d direction %q", ErrInvalidEntry, i, p.Direction)
		}
		if !p.Amount.IsPositive() {
			return fmt.Errorf("%w: posting %d amount must be positive", ErrInvalidEntry, i)
		}
		if err := p.Amount.Validate(); err != nil {
			return fmt.Errorf("%w: posting %d: %v", ErrInvalidEntry, i, err)
		}
		s, ok := sums[p.Asset]
		if !ok {
			order = append(order, p.Asset)
			s = money.Zero
		}
		if p.Direction == Debit {
			s = s.Add(p.Amount)
		} else {
			s = s.Sub(p.Amount)
		}
		sums[p.Asset] = s
	}
	for _, asset := range order {
		if !sums[asset].IsZero() {
			return fmt.Errorf("%w: %s debit − credit = %s", ErrUnbalanced, asset, sums[asset])
		}
	}
	return nil
}

// JournalEntry is a persisted entry with its postings.
type JournalEntry struct {
	ID             int64
	TenantID       string
	IdempotencyKey string
	Kind           string
	RefType        string
	RefID          string
	Reason         string
	CorrelationID  string
	CreatedAt      time.Time
	Postings       []Posting
	// Balances are the spot balances as they stood right after this entry
	// was posted, in (account, asset) order. Only Post fills them (the
	// outbox needs them for balance.updated); reads leave them nil.
	Balances []Balance
}

// Balance is the cached liability balance of a spot account in one asset.
type Balance struct {
	AccountID string
	Asset     string
	Available money.Amount
	Hold      money.Amount
	Version   int64
	UpdatedAt time.Time
}

// Total is available + hold (never stored, always derived).
func (b Balance) Total() money.Amount { return b.Available.Add(b.Hold) }

// TrialBalanceLine is one asset of the trial balance; Diff must be zero.
type TrialBalanceLine struct {
	Asset   string
	Debits  money.Amount
	Credits money.Amount
	Diff    money.Amount // debits − credits
}

// HouseBalance is a derived house-account balance, signed per the account
// type's normal balance (positive = the expected sign).
type HouseBalance struct {
	Code    HouseCode
	Type    AccountType
	Asset   string
	Balance money.Amount
}
