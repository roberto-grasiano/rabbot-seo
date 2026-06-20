package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Sentinel errors (contracts §8.1).
var (
	ErrNotFound       = errors.New("rabbot: not found")
	ErrSiteExists     = errors.New("rabbot: site already exists")
	ErrMigrationDirty = errors.New("rabbot: migration state dirty")
)

// isUniqueViolation reports whether err is (or wraps) a modernc SQLite error
// for a UNIQUE-constraint failure. modernc.org/sqlite surfaces driver errors as
// *sqlitedrv.Error whose Code() returns the SQLite result code, with extended
// result codes enabled by default, so a UNIQUE clash yields the extended code
// SQLITE_CONSTRAINT_UNIQUE (2067). We deliberately match ONLY that extended code
// — never the bare primary SQLITE_CONSTRAINT (19) — so that other constraint
// subtypes (NOT NULL, FOREIGN KEY, CHECK, …), which collapse to the same primary
// code, can never be misclassified as a duplicate and mislabeled (e.g. as a
// caller-fault HTTP 400) when they are in fact an internal fault. Every
// constraint error that is not specifically UNIQUE propagates unchanged.
func isUniqueViolation(err error) bool {
	var serr *sqlitedrv.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

var hookOnce sync.Once

// registerPragmaHook installs the connection hook that pins the contract
// PRAGMAs on every new connection. It MUST run before the first Open.
func registerPragmaHook() {
	hookOnce.Do(func() {
		sqlitedrv.RegisterConnectionHook(func(conn sqlitedrv.ExecQuerierContext, dsn string) error {
			pragmas := []string{
				"PRAGMA journal_mode = WAL",
				"PRAGMA synchronous = NORMAL",
				"PRAGMA foreign_keys = ON",
				"PRAGMA busy_timeout = 5000",
			}
			for _, p := range pragmas {
				if _, err := conn.ExecContext(context.Background(), p, nil); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// openPools opens the writer (single conn) and reader (small pool) sql.DBs
// against the same SQLite file. The PRAGMA hook must already be registered.
func openPools(dsn string) (writeDB, readDB *sql.DB, err error) {
	writeDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		_ = writeDB.Close()
		return nil, nil, err
	}
	readDB.SetMaxOpenConns(4)
	return writeDB, readDB, nil
}
