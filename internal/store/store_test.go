package store

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenRunsMigrationsAndPragmas(t *testing.T) {
	db := openTestDB(t)

	// WAL mode must be set via the connection hook.
	var jm string
	if err := db.Read().QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if jm != "wal" {
		t.Errorf("journal_mode = %q, want wal", jm)
	}

	// foreign_keys must be ON.
	var fk int
	if err := db.Read().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	// All schema tables must exist.
	wantTables := []string{
		"sites", "urls", "snapshots", "changes", "issues",
		"alerts", "segments", "url_segments", "file_snapshots", "schema_migrations",
	}
	for _, tbl := range wantTables {
		var name string
		err := db.Read().QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", tbl, err)
		}
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db1, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = db1.Close()

	// Reopen the same file: migrations should not re-run or error.
	db2, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	var n int
	if err := db2.Read().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	want := countEmbeddedMigrations(t)
	if n != want {
		t.Errorf("schema_migrations count = %d, want %d", n, want)
	}
}

// countEmbeddedMigrations returns the number of embedded *.sql migration files,
// so the idempotency assertion tracks the real migration set instead of a
// hardcoded count that must be bumped on every new migration.
func countEmbeddedMigrations(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

// TestSiteVerificationColumns asserts migration 0003 added the five
// proof-of-control columns to the sites table with the expected types and
// defaults, and that schema_migrations now tracks three migrations.
func TestSiteVerificationColumns(t *testing.T) {
	db := openTestDB(t)

	type colInfo struct {
		typ       string
		notNull   int
		dfltValue *string
	}
	want := map[string]colInfo{
		"verification_method": {typ: "TEXT", notNull: 1},
		"verification_token":  {typ: "TEXT", notNull: 1},
		"verification_state":  {typ: "TEXT", notNull: 1},
		"verified_at":         {typ: "TIMESTAMP", notNull: 0},
		"last_reverified_at":  {typ: "TIMESTAMP", notNull: 0},
	}

	rows, err := db.Read().Query("PRAGMA table_info(sites)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	got := map[string]colInfo{}
	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue *string
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		got[name] = colInfo{typ: typ, notNull: notNull, dfltValue: dfltValue}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("sites is missing column %q", name)
			continue
		}
		if g.typ != w.typ {
			t.Errorf("column %q type = %q, want %q", name, g.typ, w.typ)
		}
		if g.notNull != w.notNull {
			t.Errorf("column %q notNull = %d, want %d", name, g.notNull, w.notNull)
		}
	}

	// The text columns must default to their security-relevant constants.
	assertDefault := func(name, want string) {
		g := got[name]
		if g.dfltValue == nil {
			t.Errorf("column %q has NULL default, want %q", name, want)
			return
		}
		// SQLite stores string defaults wrapped in single quotes.
		norm := strings.Trim(*g.dfltValue, "'")
		if norm != want {
			t.Errorf("column %q default = %q, want %q", name, norm, want)
		}
	}
	assertDefault("verification_method", "")
	assertDefault("verification_token", "")
	assertDefault("verification_state", "throttled")

	// The nullable timestamps must have no default (NULL until first verify).
	for _, name := range []string{"verified_at", "last_reverified_at"} {
		if g := got[name]; g.dfltValue != nil {
			t.Errorf("column %q default = %q, want NULL", name, *g.dfltValue)
		}
	}

	var n int
	if err := db.Read().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	// Track the real embedded migration set rather than a hardcoded count, so a
	// new forward-only migration does not falsely fail this verification-column
	// test (which only cares that migration 0003 applied).
	if want := countEmbeddedMigrations(t); n != want {
		t.Errorf("schema_migrations count = %d, want %d", n, want)
	}
}

func TestWriteTxBeginImmediate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	err := db.WriteTx(ctx, func(tx Tx) error {
		_, e := tx.ExecContext(ctx,
			"INSERT INTO sites (base_url, name, created_at, updated_at) VALUES (?,?,?,?)",
			"https://example.com", "Example", "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		return e
	})
	if err != nil {
		t.Fatalf("WriteTx: %v", err)
	}
	var name string
	if err := db.Read().QueryRow("SELECT name FROM sites WHERE base_url=?", "https://example.com").Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Example" {
		t.Errorf("name = %q, want Example", name)
	}
}

// TestWriteTxCancelledCtxDoesNotPoisonWriter reproduces F2: when fn returns an
// error while ctx is already cancelled, the ROLLBACK must still reach SQLite so
// the single pooled writer connection is not returned still inside an open
// BEGIN IMMEDIATE transaction. If it were, the very next WriteTx's BEGIN
// IMMEDIATE would fail with "cannot start a transaction within a transaction"
// and every subsequent write would be permanently wedged.
func TestWriteTxCancelledCtxDoesNotPoisonWriter(t *testing.T) {
	db := openTestDB(t)

	sentinel := errors.New("boom")

	// BEGIN IMMEDIATE must succeed on a live ctx; the ctx is then cancelled while
	// fn runs, so the deferred ROLLBACK Exec would be short-circuited by
	// database/sql with ctx.Err() and never reach SQLite — leaving the pooled
	// conn inside an open transaction.
	ctx, cancel := context.WithCancel(context.Background())
	err := db.WriteTx(ctx, func(tx Tx) error {
		cancel() // cancel after BEGIN IMMEDIATE has already executed
		return sentinel
	})
	if err == nil {
		t.Fatalf("WriteTx with failing fn returned nil error, want non-nil")
	}

	// The poisoned-connection symptom only shows on the NEXT write: if the
	// rollback was skipped, the single pooled conn is still in a transaction and
	// this BEGIN IMMEDIATE fails.
	okCtx := context.Background()
	err = db.WriteTx(okCtx, func(tx Tx) error {
		_, e := tx.ExecContext(okCtx,
			"INSERT INTO sites (base_url, name, created_at, updated_at) VALUES (?,?,?,?)",
			"https://after-cancel.com", "AfterCancel", "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		return e
	})
	if err != nil {
		t.Fatalf("WriteTx after cancelled-ctx failure: %v (writer connection was poisoned)", err)
	}

	var name string
	if err := db.Read().QueryRow("SELECT name FROM sites WHERE base_url=?", "https://after-cancel.com").Scan(&name); err != nil {
		t.Fatalf("read back after recovery: %v", err)
	}
	if name != "AfterCancel" {
		t.Errorf("name = %q, want AfterCancel", name)
	}
}

func TestCheckpoint(t *testing.T) {
	db := openTestDB(t)
	if err := db.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}
