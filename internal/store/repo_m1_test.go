package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func newTestStore(t *testing.T) *store.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSnapshotAndScheduleRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite() error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://example.com/page", FirstSeen: now,
		NextCheckAt: now, Interval: 600, Importance: 1.0, Depth: 0,
		StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	})
	if err != nil {
		t.Fatalf("UpsertURL() error = %v", err)
	}

	snapID, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID: urlID, FetchedAt: now, HTTPStatus: 200, Title: "Page Title",
		ContentSHA256: "abc", ContentSimhash: 12345, Indexable: true,
		IndexabilityReason: "indexable",
	})
	if err != nil {
		t.Fatalf("SaveSnapshot() error = %v", err)
	}
	if snapID == 0 {
		t.Errorf("SaveSnapshot returned id 0")
	}

	latest, err := db.LatestSnapshot(ctx, urlID)
	if err != nil {
		t.Fatalf("LatestSnapshot() error = %v", err)
	}
	if latest.Title != "Page Title" || latest.ContentSimhash != 12345 {
		t.Errorf("LatestSnapshot mismatch: %+v", latest)
	}

	// Schedule update persists last_fetch_class plus the response ETag/Last-Modified
	// so the next crawl can issue a conditional GET.
	next := now.Add(2 * time.Hour)
	if err := db.UpdateURLSchedule(ctx, urlID, next, 1200, model.FetchOK, `"etag-1"`, "Wed, 21 Oct 2025 07:28:00 GMT"); err != nil {
		t.Fatalf("UpdateURLSchedule() error = %v", err)
	}
	got, err := db.GetURL(ctx, siteID, "https://example.com/page")
	if err != nil {
		t.Fatalf("GetURL() error = %v", err)
	}
	if got.Interval != 1200 || got.LastFetchClass != model.FetchOK {
		t.Errorf("schedule not persisted: %+v", got)
	}
	if got.ETag != `"etag-1"` || got.LastModified != "Wed, 21 Oct 2025 07:28:00 GMT" {
		t.Errorf("etag/last-modified not persisted: etag=%q lastmod=%q", got.ETag, got.LastModified)
	}
}

func TestSiteLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	id, err := db.AddSite(ctx, model.Site{BaseURL: "https://s.com", Name: "S", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite() error = %v", err)
	}
	sites, err := db.ListSites(ctx)
	if err != nil || len(sites) != 1 {
		t.Fatalf("ListSites() = %v err=%v", sites, err)
	}
	got, err := db.GetSite(ctx, id)
	if err != nil || got.BaseURL != "https://s.com" {
		t.Fatalf("GetSite() = %+v err=%v", got, err)
	}
	byURL, err := db.GetSiteByBaseURL(ctx, "https://s.com")
	if err != nil || byURL.ID != id {
		t.Fatalf("GetSiteByBaseURL() = %+v err=%v", byURL, err)
	}
	if err := db.SetSiteEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetSiteEnabled() error = %v", err)
	}
	got2, _ := db.GetSite(ctx, id)
	if got2.Enabled {
		t.Errorf("site should be disabled")
	}
}

func TestPopDueURLsOrdersByImportance(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	siteID, _ := db.AddSite(ctx, model.Site{BaseURL: "https://e.com", Name: "E", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})

	past := time.Now().UTC().Add(-time.Hour)
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://e.com/low", FirstSeen: past, NextCheckAt: past, Interval: 600, Importance: 0.2})
	_, _ = db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://e.com/high", FirstSeen: past, NextCheckAt: past, Interval: 600, Importance: 0.9})

	due, err := db.PopDueURLs(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("PopDueURLs() error = %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d, want 2", len(due))
	}
	if due[0].URL != "https://e.com/high" {
		t.Errorf("expected high-importance URL first, got %q", due[0].URL)
	}
}

func TestRecordAndQueryChanges(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	siteID, _ := db.AddSite(ctx, model.Site{BaseURL: "https://e.com", Name: "E", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})
	now := time.Now().UTC().Truncate(time.Second)
	urlID, _ := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://e.com/p", FirstSeen: now, NextCheckAt: now, Interval: 600})
	snapID, _ := db.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200})

	hist, err := db.GetURLHistory(ctx, urlID, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetURLHistory() error = %v", err)
	}
	if len(hist) != 0 {
		t.Errorf("expected empty history before any change recorded, got %d (snap %d)", len(hist), snapID)
	}
}

func TestFileSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	siteID, _ := db.AddSite(ctx, model.Site{BaseURL: "https://e.com", Name: "E", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})

	_, err := db.SaveFileSnapshot(ctx, model.FileSnapshot{
		SiteID: siteID, Kind: model.FileKindRobots, FetchedAt: time.Now().UTC(),
		ContentSHA256: "h1", ParsedEntries: "[]", HTTPStatus: 200,
	})
	if err != nil {
		t.Fatalf("SaveFileSnapshot() error = %v", err)
	}
	got, ok, err := db.LatestFileSnapshot(ctx, siteID, model.FileKindRobots)
	if err != nil || !ok {
		t.Fatalf("LatestFileSnapshot() ok=%v err=%v", ok, err)
	}
	if got.ContentSHA256 != "h1" {
		t.Errorf("file snapshot mismatch: %+v", got)
	}
}

// TestSaveFileSnapshotStoresUTC guards the store-layer UTC invariant for the
// file_snapshots.fetched_at column. modernc.org/sqlite stores a time.Time as
// wall-clock TEXT and orders it lexically, so LatestFileSnapshot ("ORDER BY
// fetched_at DESC") and TrimFileSnapshots only behave instant-correctly if
// SaveFileSnapshot normalizes to UTC at storage. We insert two snapshots: the
// one with the NEWER true instant is stamped in a west-of-UTC zone so its
// wall-clock string sorts EARLIER than the older snapshot's UTC string. Under a
// verbatim (non-UTC) store, "ORDER BY fetched_at DESC" picks the wrong row.
func TestSaveFileSnapshotStoresUTC(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	siteID, _ := db.AddSite(ctx, model.Site{BaseURL: "https://fs-utc.com", Name: "F", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})

	west := time.FixedZone("X", -8*3600)

	// Older snapshot: true instant 2026-03-31 22:00 UTC, stamped in UTC →
	// stored string "2026-03-31 22:00...".
	older := time.Date(2026, 3, 31, 22, 0, 0, 0, time.UTC)
	// Newer snapshot: true instant 2026-04-01 05:00 UTC (7h later), stamped in
	// the -8h zone → wall clock "2026-03-31 21:00", which sorts BEFORE the older
	// snapshot's string. UTC normalization at storage makes it sort AFTER.
	newer := time.Date(2026, 4, 1, 5, 0, 0, 0, time.UTC).In(west)

	if _, err := db.SaveFileSnapshot(ctx, model.FileSnapshot{
		SiteID: siteID, Kind: model.FileKindRobots, FetchedAt: older,
		ContentSHA256: "older", ParsedEntries: "[]", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("SaveFileSnapshot(older): %v", err)
	}
	if _, err := db.SaveFileSnapshot(ctx, model.FileSnapshot{
		SiteID: siteID, Kind: model.FileKindRobots, FetchedAt: newer,
		ContentSHA256: "newer", ParsedEntries: "[]", HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("SaveFileSnapshot(newer): %v", err)
	}

	got, ok, err := db.LatestFileSnapshot(ctx, siteID, model.FileKindRobots)
	if err != nil || !ok {
		t.Fatalf("LatestFileSnapshot() ok=%v err=%v", ok, err)
	}
	if got.ContentSHA256 != "newer" {
		t.Errorf("LatestFileSnapshot = %q, want %q (fetched_at not stored in UTC → lexical ordering skew)", got.ContentSHA256, "newer")
	}
	if !got.FetchedAt.Equal(newer) {
		t.Errorf("FetchedAt round-trip = %v, want instant %v", got.FetchedAt, newer)
	}
}
