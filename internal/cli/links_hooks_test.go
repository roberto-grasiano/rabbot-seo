package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedGraphDB builds a tiny site with a homepage that links to a money page, then
// syncs that out-edge set into link_edges so the link-graph reads return live data.
// It returns the open DB (caller closes), the site, and the two URL ids.
func seedGraphDB(t *testing.T) (*store.DB, model.Site, int64, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://g.test", Name: "G", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	site, err := db.GetSite(ctx, siteID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	homeID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://g.test/", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0, StatusType: model.StatusPage})
	if err != nil {
		t.Fatalf("UpsertURL(home): %v", err)
	}
	moneyID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://g.test/money", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 0.8, StatusType: model.StatusPage})
	if err != nil {
		t.Fatalf("UpsertURL(money): %v", err)
	}
	// Sync the homepage's out-edge set: home -> money.
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, homeID, now, []string{"https://g.test/money"}, 500); err != nil {
		t.Fatalf("SyncOutEdgesCapped: %v", err)
	}
	return db, site, homeID, moneyID
}

// TestLinksHook_LiveData proves the run.go links wiring is not a no-op: the hook
// resolves the URL's site, runs the blast-radius card over real link_edges, and
// returns the exact inlink figures + the ranked linker. The homepage (importance
// 1.0) is a high-importance linker, so high_importance == 1.
func TestLinksHook_LiveData(t *testing.T) {
	t.Parallel()
	db, _, _, _ := seedGraphDB(t)
	ctx := context.Background()

	hook := linksHook(db, config.Defaults().Graph)
	resp, err := hook(ctx, "https://g.test/money", 10)
	if err != nil {
		t.Fatalf("links hook: %v", err)
	}
	if resp.NotFound {
		t.Fatal("money page should be found (it has an inlink)")
	}
	if resp.Inlinks != 1 || resp.InlinkTotal != 1 {
		t.Fatalf("inlinks = %d / total %d, want 1 / 1", resp.Inlinks, resp.InlinkTotal)
	}
	if resp.HighImportance != 1 {
		t.Fatalf("high_importance = %d, want 1 (homepage importance 1.0 >= 0.70)", resp.HighImportance)
	}
	if len(resp.Linkers) != 1 || resp.Linkers[0].URL != "https://g.test/" {
		t.Fatalf("linkers = %+v, want one homepage linker", resp.Linkers)
	}
}

// TestLinksHook_UnknownURLIsNotFound proves the errors-as-data arm: a URL that no
// monitored site owns reports NotFound=true (HTTP 200), never an error.
func TestLinksHook_UnknownURLIsNotFound(t *testing.T) {
	t.Parallel()
	db, _, _, _ := seedGraphDB(t)
	ctx := context.Background()

	hook := linksHook(db, config.Defaults().Graph)
	resp, err := hook(ctx, "https://other.test/whatever", 10)
	if err != nil {
		t.Fatalf("links hook on unknown url should be data, not error: %v", err)
	}
	if !resp.NotFound {
		t.Fatalf("unknown url should be NotFound; got %+v", resp)
	}
}

// TestGraphHook_FocusLiveData proves the run.go graph wiring is not a no-op: a
// focus export over the seeded site returns the money page's neighborhood with the
// home->money edge.
func TestGraphHook_FocusLiveData(t *testing.T) {
	t.Parallel()
	db, site, _, _ := seedGraphDB(t)
	ctx := context.Background()

	hook := graphHook(db, config.Defaults().Graph)
	resp, found, err := hook(ctx, control.GraphQuery{SiteID: site.ID, Focus: "https://g.test/money", Hops: 2})
	if err != nil {
		t.Fatalf("graph hook: %v", err)
	}
	if !found {
		t.Fatal("site should be found")
	}
	if resp.Mode != "focus" {
		t.Fatalf("mode = %q, want focus", resp.Mode)
	}
	var sawEdge bool
	for _, e := range resp.Edges {
		if e.From == "https://g.test/" && e.To == "https://g.test/money" {
			sawEdge = true
		}
	}
	if !sawEdge {
		t.Fatalf("focus export missing home->money edge; edges=%+v", resp.Edges)
	}
}

// TestGraphHook_UnknownSiteIsNotFound proves the not-found-as-data arm for an
// unknown site id (HTTP 200 NotFoundResponse, not a 404/500).
func TestGraphHook_UnknownSiteIsNotFound(t *testing.T) {
	t.Parallel()
	db, _, _, _ := seedGraphDB(t)
	ctx := context.Background()

	hook := graphHook(db, config.Defaults().Graph)
	_, found, err := hook(ctx, control.GraphQuery{SiteID: 99999, Focus: "https://g.test/money"})
	if err != nil {
		t.Fatalf("graph hook on unknown site should be data, not error: %v", err)
	}
	if found {
		t.Fatal("unknown site should report found=false")
	}
}

// TestGraphHook_BadModeIsBadRequest proves a caller-fault from the export (an
// unknown mode) is wrapped as control.ErrBadRequest so the handler returns HTTP
// 400, not 500 (criterion 11).
func TestGraphHook_BadModeIsBadRequest(t *testing.T) {
	t.Parallel()
	db, site, _, _ := seedGraphDB(t)
	ctx := context.Background()

	hook := graphHook(db, config.Defaults().Graph)
	_, _, err := hook(ctx, control.GraphQuery{SiteID: site.ID, Mode: "bogus"})
	if err == nil {
		t.Fatal("an unknown export mode should error")
	}
	if !errors.Is(err, control.ErrBadRequest) {
		t.Fatalf("err = %v, want wrapped control.ErrBadRequest (so the handler returns 400)", err)
	}
}
