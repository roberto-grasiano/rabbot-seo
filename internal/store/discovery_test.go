package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestCountSiteURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	sid, _ := db.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if n, err := db.CountSiteURLs(ctx, sid); err != nil || n != 0 {
		t.Fatalf("empty: want 0, got %d err=%v", n, err)
	}
	for _, u := range []string{"https://ex.com/", "https://ex.com/a", "https://ex.com/b"} {
		if _, err := db.UpsertURL(ctx, model.URL{SiteID: sid, URL: u, FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1}); err != nil {
			t.Fatalf("UpsertURL: %v", err)
		}
	}
	if n, err := db.CountSiteURLs(ctx, sid); err != nil || n != 3 {
		t.Fatalf("want 3, got %d err=%v", n, err)
	}
}
