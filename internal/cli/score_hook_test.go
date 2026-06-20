package cli

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedScoredStore builds a site with two processed importance-1.0 URLs, a critical
// at full importance on one, and a `content` segment holding the impaired URL. It
// returns the db, the site id, and the content segment id.
func seedScoredStore(t *testing.T) (*store.DB, int64, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/k.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://a.test", Name: "A", Enabled: true, MinInterval: 600, MaxInterval: 86400})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	lc := time.Now().UTC().Add(-time.Hour)
	u1, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test/p1", FirstSeen: lc, NextCheckAt: time.Now(), Importance: 1.0, LastChecked: &lc})
	if err != nil {
		t.Fatalf("UpsertURL p1: %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test/p2", FirstSeen: lc, NextCheckAt: time.Now(), Importance: 1.0, LastChecked: &lc}); err != nil {
		t.Fatalf("UpsertURL p2: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.UpsertIssue(ctx, model.Issue{URLID: u1, RuleID: "title-missing", Status: model.IssueOpen, Severity: model.SeverityCritical, ImpactPoints: 1000, OpenedAt: now, LastSeenAt: now, Detail: "{}"}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{{SiteID: siteID, Name: "content", MatchRule: "^/p1"}})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	contentID := ids["content"]
	if err := db.SetURLSegments(ctx, u1, []int64{contentID}); err != nil {
		t.Fatalf("SetURLSegments: %v", err)
	}
	return db, siteID, contentID
}

// TestScoreHook_WholeSite asserts the daemon score hook computes the LIVE whole-site
// score and reports found=true with the coverage counts.
func TestScoreHook_WholeSite(t *testing.T) {
	db, siteID, _ := seedScoredStore(t)
	hook := scoreHook(db)

	resp, found, err := hook(context.Background(), siteID, "", time.Time{})
	if err != nil {
		t.Fatalf("scoreHook: %v", err)
	}
	if !found {
		t.Fatal("found=false, want true for a known site")
	}
	if !resp.Defined || resp.Score != 50.0 {
		t.Fatalf("score = %v defined=%v, want 50.0/true", resp.Score, resp.Defined)
	}
	if resp.KnownURLs != 2 || resp.ProcessedURLs != 2 {
		t.Fatalf("coverage = %d/%d, want 2/2", resp.KnownURLs, resp.ProcessedURLs)
	}
	if resp.OpenCritical != 1 {
		t.Fatalf("open_critical = %d, want 1", resp.OpenCritical)
	}
}

// TestScoreHook_Segment asserts a named segment scopes the score: `content` holds
// only the impaired page → score 0, and SegmentID is populated.
func TestScoreHook_Segment(t *testing.T) {
	db, siteID, contentID := seedScoredStore(t)
	hook := scoreHook(db)

	resp, found, err := hook(context.Background(), siteID, "content", time.Time{})
	if err != nil {
		t.Fatalf("scoreHook(content): %v", err)
	}
	if !found {
		t.Fatal("found=false, want true for a known segment")
	}
	if !resp.Defined || resp.Score != 0.0 {
		t.Fatalf("segment score = %v defined=%v, want 0.0/true", resp.Score, resp.Defined)
	}
	if resp.SegmentID == nil || *resp.SegmentID != contentID {
		t.Fatalf("segment id = %v, want %d", resp.SegmentID, contentID)
	}
	if resp.Segment != "content" {
		t.Fatalf("segment = %q, want content", resp.Segment)
	}
}

// TestScoreHook_UnknownSite asserts an unknown site id is errors-as-data (found=false),
// never a Go error (matching the SiteDetail pattern).
func TestScoreHook_UnknownSite(t *testing.T) {
	db, _, _ := seedScoredStore(t)
	hook := scoreHook(db)

	_, found, err := hook(context.Background(), 99999, "", time.Time{})
	if err != nil {
		t.Fatalf("unknown site must be data, not error: %v", err)
	}
	if found {
		t.Fatal("found=true for an unknown site, want false")
	}
}

// TestScoreHook_UnknownSegment asserts an unknown segment name is errors-as-data
// (found=false) on a known site — never a Go error.
func TestScoreHook_UnknownSegment(t *testing.T) {
	db, siteID, _ := seedScoredStore(t)
	hook := scoreHook(db)

	_, found, err := hook(context.Background(), siteID, "no-such-segment", time.Time{})
	if err != nil {
		t.Fatalf("unknown segment must be data, not error: %v", err)
	}
	if found {
		t.Fatal("found=true for an unknown segment, want false")
	}
}
