package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestLastCrawlAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "lc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Empty store: no crawl.
	if _, ok, err := db.LastCrawlAt(ctx); err != nil || ok {
		t.Fatalf("empty store: want (_, false, nil), got ok=%v err=%v", ok, err)
	}

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// Seeded-but-never-crawled URL (zero last_checked): still no crawl.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://ex.com/seed", FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600, Importance: 1}); err != nil {
		t.Fatalf("UpsertURL seed: %v", err)
	}
	if _, ok, err := db.LastCrawlAt(ctx); err != nil || ok {
		t.Fatalf("seeded-only: want (_, false, nil), got ok=%v err=%v", ok, err)
	}

	// A crawled URL: last_checked set on INSERT.
	crawledAt := time.Date(2026, 6, 4, 13, 29, 46, 0, time.UTC)
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://ex.com/done", FirstSeen: crawledAt, LastChecked: &crawledAt, NextCheckAt: crawledAt, Interval: 600, Importance: 1}); err != nil {
		t.Fatalf("UpsertURL done: %v", err)
	}
	got, ok, err := db.LastCrawlAt(ctx)
	if err != nil || !ok {
		t.Fatalf("crawled: want (t, true, nil), got ok=%v err=%v", ok, err)
	}
	if got.UTC().Format(time.RFC3339) != crawledAt.Format(time.RFC3339) {
		t.Errorf("LastCrawlAt = %s, want %s", got.UTC().Format(time.RFC3339), crawledAt.Format(time.RFC3339))
	}
}
