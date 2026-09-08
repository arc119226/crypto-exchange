package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Batch queues statements of different shapes into one pgx batch (one round
// trip) and scans each result into the caller's variable when Send returns.
// sqlc exports its SQL constants (emit_exported_queries), and its row structs
// list the columns in query order, so a queued sqlc statement scans by
// position into the struct sqlc generated for it.
//
// Rule for what belongs in one batch: Postgres aborts the transaction at the
// first failing statement and answers every later one with "current
// transaction is aborted", so a statement whose failure the caller must
// recover from (rolling back to a savepoint) never shares a batch with
// statements that must survive it. A :one query that returns no row is not a
// failure: Send records pgx.ErrNoRows on that result and reads on.
type Batch struct {
	b     pgx.Batch
	reads []func(pgx.BatchResults) error
}

// Result is one queued statement's outcome, filled in by Send.
type Result[T any] struct {
	Val T
	// Err is pgx.ErrNoRows when a single-row query returned nothing. Any
	// other error fails Send instead.
	Err error
}

// Sender is what pgx offers SendBatch on: pgx.Tx, *pgx.Conn, *pgxpool.Pool.
type Sender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Len is the number of queued statements.
func (b *Batch) Len() int { return b.b.Len() }

// QueueRow queues a single-row query scanned with mapper.
func QueueRow[T any](b *Batch, mapper pgx.RowToFunc[T], sql string, args ...any) *Result[T] {
	r := &Result[T]{}
	b.b.Queue(sql, args...)
	b.reads = append(b.reads, func(br pgx.BatchResults) error {
		rows, err := br.Query()
		if err != nil {
			return err
		}
		v, err := pgx.CollectOneRow(rows, mapper)
		if errors.Is(err, pgx.ErrNoRows) {
			r.Err = err
			return nil
		}
		if err != nil {
			return err
		}
		r.Val = v
		return nil
	})
	return r
}

// QueueOne queues a single-row query scanned by position into a struct.
func QueueOne[T any](b *Batch, sql string, args ...any) *Result[T] {
	return QueueRow(b, pgx.RowToStructByPos[T], sql, args...)
}

// QueueValue queues a single-row, single-column query.
func QueueValue[T any](b *Batch, sql string, args ...any) *Result[T] {
	return QueueRow(b, pgx.RowTo[T], sql, args...)
}

// QueueMany queues a multi-row query scanned by position into structs.
func QueueMany[T any](b *Batch, sql string, args ...any) *Result[[]T] {
	r := &Result[[]T]{}
	b.b.Queue(sql, args...)
	b.reads = append(b.reads, func(br pgx.BatchResults) error {
		rows, err := br.Query()
		if err != nil {
			return err
		}
		v, err := pgx.CollectRows(rows, pgx.RowToStructByPos[T])
		if err != nil {
			return err
		}
		r.Val = v
		return nil
	})
	return r
}

// QueueExec queues a statement whose result is discarded.
func (b *Batch) QueueExec(sql string, args ...any) {
	b.b.Queue(sql, args...)
	b.reads = append(b.reads, func(br pgx.BatchResults) error {
		_, err := br.Exec()
		return err
	})
}

// QueueExecRows queues a statement and records the rows it affected.
func (b *Batch) QueueExecRows(sql string, args ...any) *Result[int64] {
	r := &Result[int64]{}
	b.b.Queue(sql, args...)
	b.reads = append(b.reads, func(br pgx.BatchResults) error {
		tag, err := br.Exec()
		if err != nil {
			return err
		}
		r.Val = tag.RowsAffected()
		return nil
	})
	return r
}

// QueueExecTag queues a statement and records its command tag (a queued
// COMMIT answers ROLLBACK when the transaction had already failed).
func (b *Batch) QueueExecTag(sql string, args ...any) *Result[pgconn.CommandTag] {
	r := &Result[pgconn.CommandTag]{}
	b.b.Queue(sql, args...)
	b.reads = append(b.reads, func(br pgx.BatchResults) error {
		tag, err := br.Exec()
		if err != nil {
			return err
		}
		r.Val = tag
		return nil
	})
	return r
}

// Send runs the batch on db and fills every Result. It returns the first
// statement error; the results after it are not read (Postgres has aborted
// the transaction by then).
func (b *Batch) Send(ctx context.Context, db Sender) error {
	if b.b.Len() == 0 {
		return nil
	}
	br := db.SendBatch(ctx, &b.b)
	var first error
	for _, read := range b.reads {
		if err := read(br); err != nil {
			first = err
			break
		}
	}
	if err := br.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
