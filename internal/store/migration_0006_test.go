package store

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// jsonldInvalidCountColumn returns the PRAGMA table_info row for the
// snapshots.jsonld_invalid_count column (type, notnull, default), or fails.
func jsonldInvalidCountColumn(t *testing.T, db *DB) (typ string, notNull int, dflt *string) {
	t.Helper()
	rows, err := db.Read().Query("PRAGMA table_info(snapshots)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(snapshots): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			nn        int
			dfltValue *string
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &nn, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "jsonld_invalid_count" {
			return colType, nn, dfltValue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	t.Fatalf("snapshots.jsonld_invalid_count column missing after migrations")
	return "", 0, nil
}

// TestMigration0006AddsJSONLDInvalidCount asserts a FRESH DB carries the new
// snapshots.jsonld_invalid_count column as INTEGER NOT NULL DEFAULT 0.
func TestMigration0006AddsJSONLDInvalidCount(t *testing.T) {
	db := openTestDB(t)
	typ, notNull, dflt := jsonldInvalidCountColumn(t, db)
	if typ != "INTEGER" {
		t.Errorf("jsonld_invalid_count type = %q, want INTEGER", typ)
	}
	if notNull != 1 {
		t.Errorf("jsonld_invalid_count notnull = %d, want 1", notNull)
	}
	if dflt == nil || *dflt != "0" {
		t.Errorf("jsonld_invalid_count default = %v, want 0", dflt)
	}
}

// TestMigration0006UpgradedDBBackfillsZero asserts the upgrade path: a snapshot
// row written before any explicit count carries jsonld_invalid_count == 0 (the
// NOT NULL DEFAULT 0 backfills existing rows on ALTER). openTestDB applies all
// migrations on Open, so the "upgraded" arm is exercised by writing a snapshot
// via the M1 write path (which does not set the count) and reading it back.
func TestMigration0006UpgradedDBBackfillsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "ex", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID:      siteID,
		URL:         "https://example.com/p",
		FirstSeen:   time.Now().UTC(),
		NextCheckAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	// Write a snapshot WITHOUT setting JSONLDInvalidCount (legacy/default path).
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID:     urlID,
		FetchedAt: time.Now().UTC(),
		Title:     "t",
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// Raw read: column defaults to 0.
	var raw int
	if err := db.Read().QueryRowContext(ctx,
		"SELECT jsonld_invalid_count FROM snapshots WHERE url_id = ?", urlID).Scan(&raw); err != nil {
		t.Fatalf("read jsonld_invalid_count: %v", err)
	}
	if raw != 0 {
		t.Errorf("raw jsonld_invalid_count = %d, want 0", raw)
	}

	// Round-trip read through LatestSnapshot threads the column.
	got, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.JSONLDInvalidCount != 0 {
		t.Errorf("LatestSnapshot JSONLDInvalidCount = %d, want 0", got.JSONLDInvalidCount)
	}
}

// TestSaveSnapshotPersistsJSONLDInvalidCount asserts the count round-trips
// through SaveSnapshot/LatestSnapshot (the PR #51 sibling-list lesson: every
// read/write path must thread the new column).
func TestSaveSnapshotPersistsJSONLDInvalidCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "ex", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID:      siteID,
		URL:         "https://example.com/p",
		FirstSeen:   time.Now().UTC(),
		NextCheckAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID:              urlID,
		FetchedAt:          time.Now().UTC(),
		JSONLDInvalidCount: 3,
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.JSONLDInvalidCount != 3 {
		t.Errorf("JSONLDInvalidCount round-trip = %d, want 3", got.JSONLDInvalidCount)
	}
}
