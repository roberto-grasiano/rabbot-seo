package store

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// snapshotColumn returns the PRAGMA table_info row (type, notnull, default) for
// the named snapshots column, or fails if the column is absent.
func snapshotColumn(t *testing.T, db *DB, col string) (typ string, notNull int, dflt *string) {
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
		if name == col {
			return colType, nn, dfltValue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	t.Fatalf("snapshots.%s column missing after migrations", col)
	return "", 0, nil
}

// TestMigration0009AddsRenderModeColumns asserts a FRESH DB carries the two new
// A8 columns as TEXT NOT NULL DEFAULT ” (the migration applies cleanly on a DB
// at the current head, which openTestDB exercises by running every embedded
// migration on Open).
func TestMigration0009AddsRenderModeColumns(t *testing.T) {
	db := openTestDB(t)

	for _, col := range []string{"render_mode", "extraction_source"} {
		typ, notNull, dflt := snapshotColumn(t, db, col)
		if typ != "TEXT" {
			t.Errorf("snapshots.%s type = %q, want TEXT", col, typ)
		}
		if notNull != 1 {
			t.Errorf("snapshots.%s notnull = %d, want 1", col, notNull)
		}
		// SQLite stores a string default wrapped in single quotes; the DEFAULT ''
		// must backfill pre-A8 rows to the empty (Unknown) sentinel.
		if dflt == nil {
			t.Errorf("snapshots.%s has NULL default, want '' ('')", col)
			continue
		}
		if norm := *dflt; norm != "''" {
			t.Errorf("snapshots.%s default = %q, want \"''\"", col, norm)
		}
	}
}

// TestMigration0009UpgradedRowBackfillsEmpty asserts the no-baseline upgrade
// arm: a snapshot written WITHOUT setting RenderMode/ExtractionSource (the
// pre-A8 / disabled-hydration write path) reads back with both fields empty.
// model.RenderMode("") is the Unknown sentinel that surfaces as "unknown" on
// render surfaces. We assert both the raw column read AND the LatestSnapshot
// round-trip so a forgotten scan destination cannot pass silently.
func TestMigration0009UpgradedRowBackfillsEmpty(t *testing.T) {
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

	// Write a snapshot WITHOUT setting RenderMode/ExtractionSource (legacy path).
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID:     urlID,
		FetchedAt: time.Now().UTC(),
		Title:     "t",
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// Raw read: both columns default to ''.
	var (
		rawMode string
		rawSrc  string
	)
	if err := db.Read().QueryRowContext(ctx,
		"SELECT render_mode, extraction_source FROM snapshots WHERE url_id = ?", urlID).
		Scan(&rawMode, &rawSrc); err != nil {
		t.Fatalf("read render_mode/extraction_source: %v", err)
	}
	if rawMode != "" {
		t.Errorf("raw render_mode = %q, want \"\"", rawMode)
	}
	if rawSrc != "" {
		t.Errorf("raw extraction_source = %q, want \"\"", rawSrc)
	}

	// Round-trip read through LatestSnapshot threads both columns.
	got, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.RenderMode != model.RenderMode("") {
		t.Errorf("LatestSnapshot RenderMode = %q, want \"\" (Unknown sentinel)", got.RenderMode)
	}
	if got.ExtractionSource != "" {
		t.Errorf("LatestSnapshot ExtractionSource = %q, want \"\"", got.ExtractionSource)
	}
}

// TestSaveSnapshotPersistsRenderMode asserts the has-baseline arm: non-empty
// RenderMode + ExtractionSource round-trip through SaveSnapshot/LatestSnapshot
// (PERSISTED-ENCODING lesson — assert EXACTLY what is written is read back, via
// the real write+read path that binds the new columns by ordinal position).
//
// It also guards the SIBLING-SCAN invariant: a Title written alongside the two
// new fields must still read back correctly. A scan that mis-counted the new
// columns would shift every subsequent destination and corrupt Title/RawHTML.
func TestSaveSnapshotPersistsRenderMode(t *testing.T) {
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

	want := model.Snapshot{
		URLID:              urlID,
		FetchedAt:          time.Now().UTC(),
		Title:              "Page Title",
		IndexabilityReason: "indexable",
		RenderMode:         model.RenderClientShell,
		ExtractionSource:   "dom+next_data",
		RawHTML:            []byte("<html><body>hi</body></html>"),
	}
	if _, err := db.SaveSnapshot(ctx, want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.RenderMode != model.RenderClientShell {
		t.Errorf("RenderMode round-trip = %q, want %q", got.RenderMode, model.RenderClientShell)
	}
	if got.ExtractionSource != "dom+next_data" {
		t.Errorf("ExtractionSource round-trip = %q, want %q", got.ExtractionSource, "dom+next_data")
	}
	// Sibling-scan guard: adjacent columns must not be shifted by the two inserts.
	if got.Title != "Page Title" {
		t.Errorf("Title round-trip = %q, want %q (column ordinal shift?)", got.Title, "Page Title")
	}
	if got.IndexabilityReason != "indexable" {
		t.Errorf("IndexabilityReason round-trip = %q, want %q (column ordinal shift?)", got.IndexabilityReason, "indexable")
	}
	if string(got.RawHTML) != string(want.RawHTML) {
		t.Errorf("RawHTML round-trip = %q, want %q (column ordinal shift?)", got.RawHTML, want.RawHTML)
	}
}

// TestSaveSnapshotPersistsAllRenderModes exercises every non-empty RenderMode
// value through the persisted encoding, so a typo in any enum literal or a
// store cast that drops a value is caught (PERSISTED-ENCODING + sibling-switch:
// the store must round-trip the EXACT string the classifier writes for each of
// the five render kinds).
func TestSaveSnapshotPersistsAllRenderModes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://rm.com", Name: "rm", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	modes := []model.RenderMode{
		model.RenderServerRendered,
		model.RenderHydrated,
		model.RenderHeadOnlyShell,
		model.RenderClientShell,
		model.RenderUnknown,
	}
	for i, mode := range modes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			// A distinct URL per mode so LatestSnapshot returns this row.
			u := "https://rm.com/p" + string(rune('a'+i))
			urlID, err := db.UpsertURL(ctx, model.URL{
				SiteID:      siteID,
				URL:         u,
				FirstSeen:   time.Now().UTC(),
				NextCheckAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("UpsertURL: %v", err)
			}
			if _, err := db.SaveSnapshot(ctx, model.Snapshot{
				URLID:            urlID,
				FetchedAt:        time.Now().UTC(),
				RenderMode:       mode,
				ExtractionSource: "dom",
			}); err != nil {
				t.Fatalf("SaveSnapshot(%s): %v", mode, err)
			}
			got, err := db.LatestSnapshot(ctx, urlID)
			if err != nil {
				t.Fatalf("LatestSnapshot(%s): %v", mode, err)
			}
			if got.RenderMode != mode {
				t.Errorf("RenderMode round-trip = %q, want %q", got.RenderMode, mode)
			}
		})
	}
}
