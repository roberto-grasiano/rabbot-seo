package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestBuildReport_HealthBlock_SiteScoped is A6 read-surface slice: a site-scoped
// BuildReport carries a Health block with the LIVE current score, per-segment
// scores, and the top contributing rules; a window-start score + delta when the
// persisted series has a point at/after the window lower bound.
func TestBuildReport_HealthBlock_SiteScoped(t *testing.T) {
	t.Parallel()
	db := newTestStore(t)
	ctx := context.Background()

	siteID := hsSeedSite(t, db, "health.test")
	// Two processed importance-1.0 pages; a critical at full importance on one.
	u1 := hsSeedURL(t, db, siteID, "p1", 1.0, true)
	_ = hsSeedURL(t, db, siteID, "p2", 1.0, true)
	hsOpenIssue(t, db, u1, "title-missing", model.SeverityCritical, 1000, model.IssueOpen)

	// One processed page in segment "content".
	segID := makeSegment(t, db, siteID, "content", u1)

	since := time.Now().UTC().Add(-24 * time.Hour)
	res, err := db.BuildReport(ctx, store.ReportParams{Since: since, SiteID: &siteID, TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if res.Health == nil {
		t.Fatal("site-scoped report has no Health block")
	}
	h := res.Health
	if !h.Current.Defined {
		t.Fatalf("current health undefined, want defined; %+v", h.Current)
	}
	// 1 fully-impaired page of 2 equal-importance pages → 50.0.
	if h.Current.Score != 50.0 {
		t.Fatalf("current score = %v, want 50.0", h.Current.Score)
	}
	if h.Current.KnownURLs != 2 || h.Current.ProcessedURLs != 2 {
		t.Fatalf("coverage = known %d / processed %d, want 2/2", h.Current.KnownURLs, h.Current.ProcessedURLs)
	}
	// Per-segment: segment "content" holds only u1 (the impaired page) → score 0.
	if len(h.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(h.Segments))
	}
	if h.Segments[0].Name != "content" {
		t.Fatalf("segment name = %q, want content", h.Segments[0].Name)
	}
	if h.Segments[0].SegmentID == nil || *h.Segments[0].SegmentID != segID {
		t.Fatalf("segment id = %v, want %d", h.Segments[0].SegmentID, segID)
	}
	if !h.Segments[0].Defined || h.Segments[0].Score != 0.0 {
		t.Fatalf("segment health = %+v, want defined score 0", h.Segments[0])
	}
	// Top contributing rules: the critical's rule, with its (uncapped) mass.
	if len(h.TopRules) == 0 || h.TopRules[0].RuleID != "title-missing" {
		t.Fatalf("top rules = %+v, want title-missing first", h.TopRules)
	}
	if h.TopRules[0].Mass != 1000 {
		t.Fatalf("top rule mass = %d, want 1000", h.TopRules[0].Mass)
	}
}

// TestBuildReport_HealthBlock_WindowStartDelta locks the window-start score and
// delta: with a persisted trend point at/after the window lower bound, the block
// reports the window-start score and the delta from it to the live current score.
func TestBuildReport_HealthBlock_WindowStartDelta(t *testing.T) {
	t.Parallel()
	db := newTestStore(t)
	ctx := context.Background()

	siteID := hsSeedSite(t, db, "delta.test")
	u1 := hsSeedURL(t, db, siteID, "p1", 1.0, true)
	hsSeedURL(t, db, siteID, "p2", 1.0, true)

	// Persist a clean (score 100) point inside the window, then open a critical and
	// persist the dropped point (score 50). Live current = 50.
	t0 := time.Now().UTC().Add(-12 * time.Hour)
	if err := db.RecordHealthScores(ctx, siteID, u1, t0); err != nil {
		t.Fatalf("RecordHealthScores(clean): %v", err)
	}
	hsOpenIssue(t, db, u1, "title-missing", model.SeverityCritical, 1000, model.IssueOpen)
	if err := db.RecordHealthScores(ctx, siteID, u1, t0.Add(time.Hour)); err != nil {
		t.Fatalf("RecordHealthScores(dropped): %v", err)
	}

	since := time.Now().UTC().Add(-24 * time.Hour)
	res, err := db.BuildReport(ctx, store.ReportParams{Since: since, SiteID: &siteID, TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	h := res.Health
	if h == nil || !h.Current.Defined {
		t.Fatalf("Health block missing/undefined: %+v", h)
	}
	if h.Current.Score != 50.0 {
		t.Fatalf("current = %v, want 50", h.Current.Score)
	}
	if h.WindowStart == nil {
		t.Fatal("WindowStart nil, want the 100.0 window-start point")
	}
	if *h.WindowStart != 100.0 {
		t.Fatalf("WindowStart = %v, want 100.0", *h.WindowStart)
	}
	if h.Delta == nil || *h.Delta != -50.0 {
		t.Fatalf("Delta = %v, want -50.0", h.Delta)
	}
}

// TestBuildReport_HealthBlock_AllSites: an all-sites report carries a LIVE score per
// SiteRollup row, defined per site independently; an undefined (cold-start) site
// reports Defined=false (renderers show "—", never 100/0).
func TestBuildReport_HealthBlock_AllSites(t *testing.T) {
	t.Parallel()
	db := newTestStore(t)
	ctx := context.Background()

	// Site A: processed, with an open issue → defined, < 100. Make it "active" so the
	// rollup includes it (a rollup row needs a change or an open issue).
	siteA := hsSeedSite(t, db, "a.test")
	a1 := hsSeedURL(t, db, siteA, "p1", 1.0, true)
	hsOpenIssue(t, db, a1, "title-missing", model.SeverityCritical, 1000, model.IssueOpen)

	res, err := db.BuildReport(ctx, store.ReportParams{Since: time.Now().UTC().Add(-24 * time.Hour), TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if res.Health != nil {
		t.Fatalf("all-sites report should not carry the site Health block; got %+v", res.Health)
	}
	var rowA *store.SiteRollup
	for i := range res.Sites {
		if res.Sites[i].SiteID == siteA {
			rowA = &res.Sites[i]
		}
	}
	if rowA == nil {
		t.Fatalf("site A missing from rollups: %+v", res.Sites)
	}
	if !rowA.Health.Defined {
		t.Fatalf("site A health undefined, want defined: %+v", rowA.Health)
	}
	if rowA.Health.Score != 0.0 {
		t.Fatalf("site A score = %v, want 0.0 (one fully-impaired importance-1.0 page)", rowA.Health.Score)
	}
}

// makeSegment creates a segment for siteID and adds the given URLs as members,
// returning the segment id.
func makeSegment(t *testing.T, db *store.DB, siteID int64, name string, urlIDs ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{{Name: name, MatchRule: "^/"}})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	segID := ids[name]
	for _, uid := range urlIDs {
		if err := db.SetURLSegments(ctx, uid, []int64{segID}); err != nil {
			t.Fatalf("SetURLSegments(%d): %v", uid, err)
		}
	}
	return segID
}
