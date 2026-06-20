// Package store owns all SQLite access: a single-writer pool plus a read pool,
// the contract PRAGMAs via a connection hook, and embedded migrations.
//
// The concrete type is *store.DB (per the cross-plan amendment §A). Open returns
// *DB; downstream constructors take *DB (concrete), never a Store interface, so
// repository methods can be added incrementally across M1–M3 without breaking
// compilation. M0 implements only Open/Read/WriteTx/Checkpoint/Close + the
// migrations runner; entity repositories arrive in later milestones.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// Tx is the minimal write-transaction surface used by repositories.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB holds the writer and reader pools for one SQLite database file.
type DB struct {
	writeDB *sql.DB
	readDB  *sql.DB
}

// Open registers the PRAGMA hook (once), opens both pools against
// file:<path>, verifies connectivity, and runs embedded migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	registerPragmaHook()
	dsn := "file:" + path
	writeDB, readDB, err := openPools(dsn)
	if err != nil {
		return nil, err
	}
	if err := writeDB.PingContext(ctx); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, err
	}
	if err := runMigrations(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, err
	}
	return &DB{writeDB: writeDB, readDB: readDB}, nil
}

// Read returns the read pool for queries (CLI/TUI/read-only callers).
func (db *DB) Read() *sql.DB { return db.readDB }

// WriteTx runs fn inside a BEGIN IMMEDIATE transaction on the single writer,
// acquiring the write lock up front. Commits on nil error, rolls back otherwise.
func (db *DB) WriteTx(ctx context.Context, fn func(tx Tx) error) error {
	conn, err := db.writeDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(connTx{conn: conn}); err != nil {
		rollback(ctx, conn)
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		rollback(ctx, conn)
		return err
	}
	return nil
}

// rollback aborts the open BEGIN IMMEDIATE transaction on conn. It deliberately
// runs the ROLLBACK on a non-cancellable context: if WriteTx's caller ctx is
// already cancelled or past its deadline when we get here, database/sql would
// short-circuit the Exec with ctx.Err() and never send ROLLBACK to SQLite,
// returning the single pooled writer connection to the pool still inside a
// transaction — which permanently wedges every subsequent write with "cannot
// start a transaction within a transaction". context.WithoutCancel detaches the
// cancellation/deadline so the abort always reaches the engine. If the rollback
// still fails, the connection is force-discarded (driver.ErrBadConn via
// conn.Raw) so a poisoned connection can never be reused.
func rollback(ctx context.Context, conn *sql.Conn) {
	rbCtx := context.WithoutCancel(ctx)
	if _, err := conn.ExecContext(rbCtx, "ROLLBACK"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// connTx adapts a *sql.Conn (already inside BEGIN IMMEDIATE) to the Tx surface.
type connTx struct {
	conn *sql.Conn
}

func (t connTx) ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, q, a...)
}
func (t connTx) QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, q, a...)
}
func (t connTx) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, q, a...)
}

// ErrCheckpointBusy reports that a wal_checkpoint could not run to completion
// because a reader or writer still held the WAL — the checkpoint made no (or
// only partial) progress and should be retried. WAL growth is bounded by
// SQLite's auto-checkpoint, so a busy result is a soft failure, not data loss.
var ErrCheckpointBusy = errors.New("rabbot: wal checkpoint busy")

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) on the writer so the -wal
// file growth stays bounded under a 24/7 writer + long-lived readers. The pragma
// returns a (busy, log, checkpointed) row; a busy=1 result means the checkpoint
// was blocked and reports no real success, so it is surfaced as ErrCheckpointBusy
// instead of being silently discarded.
func (db *DB) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := db.writeDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return ErrCheckpointBusy
	}
	return nil
}

// Close closes both pools.
func (db *DB) Close() error {
	rerr := db.readDB.Close()
	werr := db.writeDB.Close()
	if werr != nil {
		return werr
	}
	return rerr
}
