package marketdata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/marketdata/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Store is the database side of market data: the engine's tables read for
// snapshots and the trade tape, and marketdata.klines read and written.
type Store struct {
	pool   *pgxpool.Pool
	tenant string
}

// NewStore wraps a pool. Reads need SELECT on trading.*, ledger.accounts and
// marketdata.*; ApplyCandles needs the worker's INSERT/UPDATE on marketdata.*.
func NewStore(pool *pgxpool.Pool, tenant string) *Store {
	return &Store{pool: pool, tenant: tenant}
}

// Reader is what the api role serves klines and the ticker from.
type Reader interface {
	Klines(ctx context.Context, market string, iv Interval, from, to time.Time, limit int32) ([]Candle, error)
	Ticker(ctx context.Context, market string, now time.Time) (Ticker, error)
}

var _ Reader = (*Store)(nil)

// readOnly runs fn in a REPEATABLE READ, READ ONLY transaction: every query
// inside sees one committed state, so a seq and the rows it describes agree.
func (s *Store) readOnly(ctx context.Context, fn func(q *sqlcgen.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read only
	if err := fn(sqlcgen.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OpenOrders is the shadow book's snapshot: the market's resting orders and
// the engine seq they are current at, read atomically.
func (s *Store) OpenOrders(ctx context.Context, marketID, symbol string) (BookSnapshot, error) {
	snap := BookSnapshot{Market: symbol}
	err := s.readOnly(ctx, func(q *sqlcgen.Queries) error {
		seq, err := q.GetMarketLastSeq(ctx, marketID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("marketdata: market seq: %w", err)
		}
		snap.Seq = uint64(max(seq, 0)) //nolint:gosec // last_seq >= 0 by CHECK
		rows, err := q.ListOpenOrdersForBook(ctx, marketID)
		if err != nil {
			return fmt.Errorf("marketdata: open orders: %w", err)
		}
		snap.Orders = make([]RestingOrder, 0, len(rows))
		for _, r := range rows {
			price, err := pg.AmountFromNumeric(r.Price)
			if err != nil {
				return fmt.Errorf("marketdata: order %s price: %w", r.ID, err)
			}
			rem, err := pg.AmountFromNumeric(r.RemainingQty)
			if err != nil {
				return fmt.Errorf("marketdata: order %s remaining: %w", r.ID, err)
			}
			snap.Orders = append(snap.Orders, RestingOrder{ID: r.ID, Side: Side(r.Side), Price: price, Remaining: rem})
		}
		return nil
	})
	return snap, err
}

// MarketSeq is the engine's last committed seq for a market (0 before its
// first command).
func (s *Store) MarketSeq(ctx context.Context, marketID string) (int64, error) {
	seq, err := sqlcgen.New(s.pool).GetMarketLastSeq(ctx, marketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("marketdata: market seq: %w", err)
	}
	return seq, nil
}

// TradesBetween returns the trades of commands in (afterSeq, uptoSeq], in
// execution order.
func (s *Store) TradesBetween(ctx context.Context, marketID string, afterSeq, uptoSeq int64) ([]TradeTick, error) {
	return tradesBetween(ctx, sqlcgen.New(s.pool), marketID, afterSeq, uptoSeq)
}

func tradesBetween(ctx context.Context, q *sqlcgen.Queries, marketID string, afterSeq, uptoSeq int64) ([]TradeTick, error) {
	rows, err := q.ListTradesBetweenSeq(ctx, sqlcgen.ListTradesBetweenSeqParams{MarketID: marketID, AfterSeq: afterSeq, UptoSeq: uptoSeq})
	if err != nil {
		return nil, fmt.Errorf("marketdata: trades: %w", err)
	}
	out := make([]TradeTick, 0, len(rows))
	for _, r := range rows {
		price, err := pg.AmountFromNumeric(r.Price)
		if err != nil {
			return nil, fmt.Errorf("marketdata: trade %s price: %w", r.ID, err)
		}
		qty, err := pg.AmountFromNumeric(r.Qty)
		if err != nil {
			return nil, fmt.Errorf("marketdata: trade %s qty: %w", r.ID, err)
		}
		quote, err := pg.AmountFromNumeric(r.QuoteQty)
		if err != nil {
			return nil, fmt.Errorf("marketdata: trade %s quote: %w", r.ID, err)
		}
		out = append(out, TradeTick{
			TradeID: r.ID, Market: r.MarketSymbol, Seq: uint64(r.Seq), Index: int(r.Idx), //nolint:gosec // seq >= 0
			Price: price, Qty: qty, QuoteQty: quote, TakerSide: Side(r.TakerSide), At: r.CreatedAt,
		})
	}
	return out, nil
}

// KlineCursor is the last seq the writer folded for a market (0 = none).
func (s *Store) KlineCursor(ctx context.Context, market string) (int64, error) {
	cur, err := sqlcgen.New(s.pool).GetKlineCursor(ctx, sqlcgen.GetKlineCursorParams{TenantID: s.tenant, MarketSymbol: market})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("marketdata: kline cursor: %w", err)
	}
	return cur, nil
}

// ApplyCandles folds a batch of candles into the stored ones and moves the
// market's cursor to cursor, in one transaction. Re-running a batch whose
// cursor was already committed is refused rather than double counted.
func (s *Store) ApplyCandles(ctx context.Context, market string, candles []Candle, cursor int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back on any error below
	q := sqlcgen.New(tx)
	have, err := q.GetKlineCursor(ctx, sqlcgen.GetKlineCursorParams{TenantID: s.tenant, MarketSymbol: market})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("marketdata: kline cursor: %w", err)
	}
	if have >= cursor {
		return fmt.Errorf("marketdata: kline cursor for %s is already at %d, batch ends at %d", market, have, cursor)
	}
	for _, c := range candles {
		if c.Trades == 0 {
			continue
		}
		if err := q.UpsertKline(ctx, sqlcgen.UpsertKlineParams{
			TenantID: s.tenant, MarketSymbol: market, Interval: string(c.Interval), BucketStart: c.Start,
			Open: pg.NumericFromAmount(c.Open), High: pg.NumericFromAmount(c.High), Low: pg.NumericFromAmount(c.Low), Close: pg.NumericFromAmount(c.Close),
			Volume: pg.NumericFromAmount(c.Volume), QuoteVolume: pg.NumericFromAmount(c.QuoteVolume), Trades: int32(c.Trades), //nolint:gosec // a batch holds far fewer trades than 2^31
		}); err != nil {
			return fmt.Errorf("marketdata: upsert kline: %w", err)
		}
	}
	if err := q.UpsertKlineCursor(ctx, sqlcgen.UpsertKlineCursorParams{TenantID: s.tenant, MarketSymbol: market, LastSeq: cursor}); err != nil {
		return fmt.Errorf("marketdata: upsert kline cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketdata: commit: %w", err)
	}
	return nil
}

// Klines returns the stored candles of [from, to), ascending, at most
// limit. Gaps are the caller's business (FillGaps).
func (s *Store) Klines(ctx context.Context, market string, iv Interval, from, to time.Time, limit int32) ([]Candle, error) {
	return klines(ctx, sqlcgen.New(s.pool), s.tenant, market, iv, from, to, limit)
}

func klines(ctx context.Context, q *sqlcgen.Queries, tenant, market string, iv Interval, from, to time.Time, limit int32) ([]Candle, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := q.ListKlines(ctx, sqlcgen.ListKlinesParams{
		TenantID: tenant, MarketSymbol: market, Interval: string(iv), FromStart: from.UTC(), ToStart: to.UTC(), RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("marketdata: list klines: %w", err)
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		c, err := candleFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func candleFromRow(r sqlcgen.ListKlinesRow) (Candle, error) {
	var (
		c   = Candle{Market: r.MarketSymbol, Interval: Interval(r.Interval), Start: r.BucketStart, Trades: int64(r.Trades)}
		err error
	)
	if c.Open, err = pg.AmountFromNumeric(r.Open); err != nil {
		return c, fmt.Errorf("marketdata: kline open: %w", err)
	}
	if c.High, err = pg.AmountFromNumeric(r.High); err != nil {
		return c, fmt.Errorf("marketdata: kline high: %w", err)
	}
	if c.Low, err = pg.AmountFromNumeric(r.Low); err != nil {
		return c, fmt.Errorf("marketdata: kline low: %w", err)
	}
	if c.Close, err = pg.AmountFromNumeric(r.Close); err != nil {
		return c, fmt.Errorf("marketdata: kline close: %w", err)
	}
	if c.Volume, err = pg.AmountFromNumeric(r.Volume); err != nil {
		return c, fmt.Errorf("marketdata: kline volume: %w", err)
	}
	if c.QuoteVolume, err = pg.AmountFromNumeric(r.QuoteVolume); err != nil {
		return c, fmt.Errorf("marketdata: kline quote volume: %w", err)
	}
	return c, nil
}

// LastCloseBefore is the close of the last stored candle of iv before t,
// if any: what FillGaps carries into a range that starts in a gap.
func (s *Store) LastCloseBefore(ctx context.Context, market string, iv Interval, t time.Time) (*money.Amount, error) {
	n, err := sqlcgen.New(s.pool).LastKlineBefore(ctx, sqlcgen.LastKlineBeforeParams{
		TenantID: s.tenant, MarketSymbol: market, Interval: string(iv), BucketStart: t.UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no earlier candle is a value, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("marketdata: last kline: %w", err)
	}
	a, err := pg.AmountFromNumeric(n)
	if err != nil {
		return nil, fmt.Errorf("marketdata: last close: %w", err)
	}
	return &a, nil
}

// Ticker folds the last 24 hours of 1m candles.
func (s *Store) Ticker(ctx context.Context, market string, now time.Time) (Ticker, error) {
	candles, err := s.Klines(ctx, market, Interval1m, now.Add(-TickerWindow), now, 24*60+1)
	if err != nil {
		return Ticker{}, err
	}
	return TickerFrom(market, candles, now), nil
}

// RingSeed is what the stream role starts a market's candle ring from: the
// stored 1m candles of the last day, the trades the writer had not folded
// yet, and the engine seq all of it is current at. Live trades with a seq
// at or below Seq are already inside.
type RingSeed struct {
	Candles []Candle
	Trades  []TradeTick
	Seq     int64
}

// RingSeed reads the seed atomically. The cursor, the candles and the trade
// tape come from one snapshot, so nothing between them is double counted.
func (s *Store) RingSeed(ctx context.Context, marketID, symbol string, now time.Time) (RingSeed, error) {
	var seed RingSeed
	err := s.readOnly(ctx, func(q *sqlcgen.Queries) error {
		seq, err := q.GetMarketLastSeq(ctx, marketID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("marketdata: market seq: %w", err)
		}
		seed.Seq = seq
		cursor, err := q.GetKlineCursor(ctx, sqlcgen.GetKlineCursorParams{TenantID: s.tenant, MarketSymbol: symbol})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("marketdata: kline cursor: %w", err)
		}
		if seed.Candles, err = klines(ctx, q, s.tenant, symbol, Interval1m, now.Add(-TickerWindow), now.Add(time.Hour), 24*60+61); err != nil {
			return err
		}
		if cursor < seq {
			if seed.Trades, err = tradesBetween(ctx, q, marketID, cursor, seq); err != nil {
				return err
			}
		}
		return nil
	})
	return seed, err
}

// AccountSeq is where an account's private event sequence stands.
func (s *Store) AccountSeq(ctx context.Context, accountID string) (int64, error) {
	seq, err := sqlcgen.New(s.pool).GetAccountSeq(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("marketdata: account %s: %w", accountID, ErrAccountNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("marketdata: account seq: %w", err)
	}
	return seq, nil
}

// ErrAccountNotFound is an account id the ledger does not know.
var ErrAccountNotFound = errors.New("marketdata: account not found")
