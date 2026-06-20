package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// gwSite seeds a site and returns its id.
func gwSite(t *testing.T, db *store.DB, host string) int64 {
	t.Helper()
	id, err := db.AddSite(context.Background(), model.Site{
		BaseURL: "https://" + host, Name: host, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", host, err)
	}
	return id
}

// gwURL inserts a URL whose last_checked is set when crawled=true (the "fetched
// at least once" marker) and NULL otherwise (admitted but never crawled).
func gwURL(t *testing.T, db *store.DB, siteID int64, path string, crawled bool) {
	t.Helper()
	now := time.Now().UTC()
	var lc *time.Time
	if crawled {
		c := now.Add(-time.Minute)
		lc = &c
	}
	if _, err := db.UpsertURL(context.Background(), model.URL{
		SiteID: siteID, URL: "https://gw/" + path, FirstSeen: now, LastChecked: lc,
		NextCheckAt: now, Interval: 600, Importance: 0.5,
		StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	}); err != nil {
		t.Fatalf("UpsertURL(%q): %v", path, err)
	}
}

// TestGraphWarmFalseWhilePagesUncrawled: a site with at least one admitted-but-
// never-crawled URL (last_checked IS NULL) is NOT warm — the inlink picture is
// still partial, so orphan signals are not trustworthy yet. This is the heart of
// the #83 cold-start gate.
func TestGraphWarmFalseWhilePagesUncrawled(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	siteID := gwSite(t, db, "partial.example")

	gwURL(t, db, siteID, "a", true)  // crawled
	gwURL(t, db, siteID, "b", false) // admitted, never crawled → NULL last_checked

	warm, err := db.GraphWarm(ctx, siteID)
	if err != nil {
		t.Fatalf("GraphWarm: %v", err)
	}
	if warm {
		t.Fatalf("GraphWarm = true while a URL is still uncrawled (last_checked NULL); want false")
	}
}

// TestGraphWarmTrueWhenAllCrawled: once every admitted URL has been fetched at
// least once (no last_checked IS NULL), the first full crawl is complete and the
// graph is warm.
func TestGraphWarmTrueWhenAllCrawled(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	siteID := gwSite(t, db, "complete.example")

	gwURL(t, db, siteID, "a", true)
	gwURL(t, db, siteID, "b", true)

	warm, err := db.GraphWarm(ctx, siteID)
	if err != nil {
		t.Fatalf("GraphWarm: %v", err)
	}
	if !warm {
		t.Fatalf("GraphWarm = false when every URL has been crawled; want true")
	}
}

// TestGraphWarmTrueWhenNoURLs: a site with zero admitted URLs is vacuously warm —
// there are no targets to orphan, so the gate must not block on an empty site.
func TestGraphWarmTrueWhenNoURLs(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	siteID := gwSite(t, db, "empty.example")

	warm, err := db.GraphWarm(ctx, siteID)
	if err != nil {
		t.Fatalf("GraphWarm: %v", err)
	}
	if !warm {
		t.Fatalf("GraphWarm = false on a site with no URLs; want true (vacuously warm)")
	}
}

// TestGraphWarmStoreErrorSurfaced: a read failure (closed DB) is surfaced as an
// error, never silently reported as warm — a swallowed error would let the
// cold-start gate mis-open during a partial crawl.
func TestGraphWarmStoreErrorSurfaced(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	siteID := gwSite(t, db, "err.example")
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	warm, err := db.GraphWarm(ctx, siteID)
	if err == nil {
		t.Fatalf("GraphWarm on a closed DB returned nil error, want a surfaced read error")
	}
	if warm {
		t.Fatalf("GraphWarm reported warm=true on a read error; want false")
	}
}

// TestGraphWarmIsPerSite: one site being partial must not drag a sibling site's
// warm state down (the COUNT is scoped to site_id).
func TestGraphWarmIsPerSite(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	warmSite := gwSite(t, db, "warm.example")
	coldSite := gwSite(t, db, "cold.example")

	gwURL(t, db, warmSite, "a", true)  // warm site: all crawled
	gwURL(t, db, coldSite, "a", false) // cold site: uncrawled

	warm, err := db.GraphWarm(ctx, warmSite)
	if err != nil {
		t.Fatalf("GraphWarm(warm): %v", err)
	}
	if !warm {
		t.Fatalf("warm site reported cold because a sibling site is partial; want warm")
	}
	cold, err := db.GraphWarm(ctx, coldSite)
	if err != nil {
		t.Fatalf("GraphWarm(cold): %v", err)
	}
	if cold {
		t.Fatalf("cold site reported warm; want cold")
	}
}
