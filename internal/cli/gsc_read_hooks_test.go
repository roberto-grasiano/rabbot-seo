package cli

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestIndexStatusHookLiveData seeds a url_index_status row and asserts the hook
// reads it back with the verdict/coverage/canonical fields and an RFC3339-stamped
// inspected_at + last_crawl_time.
func TestIndexStatusHookLiveData(t *testing.T) {
	ctx := context.Background()
	db, siteID := openSeededStore(t)

	inspected := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	lastCrawl := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID:          siteID,
		URL:             "https://a.test/p",
		InspectedAt:     inspected,
		Verdict:         "PASS",
		CoverageState:   "Submitted and indexed",
		IndexingState:   "INDEXING_ALLOWED",
		RobotsTxtState:  "ALLOWED",
		PageFetchState:  "SUCCESSFUL",
		GoogleCanonical: "https://a.test/p",
		UserCanonical:   "https://a.test/p",
		CrawledAs:       "DESKTOP",
		LastCrawlTime:   &lastCrawl,
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}

	resp, err := indexStatusHook(db)(ctx, "https://a.test/p")
	if err != nil {
		t.Fatalf("indexStatusHook: %v", err)
	}
	if resp.NotFound || !resp.HasStatus {
		t.Fatalf("want HasStatus=true NotFound=false, got %+v", resp)
	}
	if resp.Verdict != "PASS" || resp.CoverageState != "Submitted and indexed" ||
		resp.GoogleCanonical != "https://a.test/p" {
		t.Fatalf("decoded resp = %+v", resp)
	}
	if resp.InspectedAt != inspected.Format(time.RFC3339) {
		t.Fatalf("inspected_at = %q, want %q", resp.InspectedAt, inspected.Format(time.RFC3339))
	}
	if resp.LastCrawlTime != lastCrawl.Format(time.RFC3339) {
		t.Fatalf("last_crawl_time = %q, want %q", resp.LastCrawlTime, lastCrawl.Format(time.RFC3339))
	}
}

// TestIndexStatusHookUnInspectedIsData is the central GSC W2 guard at the store
// boundary: a URL with NO url_index_status row reports has_status=false /
// not_found=true (the LatestURLIndexStatus ok=false contract), NEVER a discrepancy
// and never an error.
func TestIndexStatusHookUnInspectedIsData(t *testing.T) {
	ctx := context.Background()
	db, _ := openSeededStore(t)

	resp, err := indexStatusHook(db)(ctx, "https://a.test/never-inspected")
	if err != nil {
		t.Fatalf("indexStatusHook un-inspected must not error: %v", err)
	}
	if !resp.NotFound || resp.HasStatus {
		t.Fatalf("want NotFound=true HasStatus=false for an un-inspected URL, got %+v", resp)
	}
	if resp.Verdict != "" {
		t.Fatalf("verdict = %q, want empty for absent data", resp.Verdict)
	}
}

// TestIndexStatusHookEmptyLastCrawlOmitted asserts a nil LastCrawlTime reads back
// as an empty string (omitted), not a zero-time stamp.
func TestIndexStatusHookEmptyLastCrawlOmitted(t *testing.T) {
	ctx := context.Background()
	db, siteID := openSeededStore(t)

	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID:        siteID,
		URL:           "https://a.test/q",
		InspectedAt:   time.Now().UTC(),
		Verdict:       "NEUTRAL",
		CoverageState: "Crawled - currently not indexed",
		LastCrawlTime: nil,
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}
	resp, err := indexStatusHook(db)(ctx, "https://a.test/q")
	if err != nil {
		t.Fatalf("indexStatusHook: %v", err)
	}
	if resp.LastCrawlTime != "" {
		t.Fatalf("last_crawl_time = %q, want empty for a nil crawl time", resp.LastCrawlTime)
	}
}

// TestSearchPerformanceHookLiveData seeds metrics across days and queries and
// asserts the hook returns them filtered by `since` and ordered newest-first.
func TestSearchPerformanceHookLiveData(t *testing.T) {
	ctx := context.Background()
	db, siteID := openSeededStore(t)

	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteID, URL: "https://a.test/p", Query: "rabbit seo", Date: "2026-06-15", Clicks: 10, Impressions: 100, CTR: 0.1, Position: 4.2},
		{SiteID: siteID, URL: "https://a.test/p", Query: "rabbit seo", Date: "2026-06-14", Clicks: 8, Impressions: 90, CTR: 0.089, Position: 4.6},
		{SiteID: siteID, URL: "https://a.test/p", Query: "rabbit seo", Date: "2026-06-01", Clicks: 1, Impressions: 5, CTR: 0.2, Position: 9.0},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}

	since := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	resp, err := searchPerformanceHook(db)(ctx, "https://a.test/p", since)
	if err != nil {
		t.Fatalf("searchPerformanceHook: %v", err)
	}
	if !resp.HasData {
		t.Fatalf("want HasData=true, got %+v", resp)
	}
	// The 2026-06-01 row is before `since` and must be excluded; the remaining two
	// are newest-first.
	if len(resp.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (06-01 excluded by since)", len(resp.Rows))
	}
	if resp.Rows[0].Date != "2026-06-15" || resp.Rows[1].Date != "2026-06-14" {
		t.Fatalf("rows not newest-first: %+v", resp.Rows)
	}
	if resp.Rows[0].Impressions != 100 {
		t.Fatalf("row impressions = %d, want 100", resp.Rows[0].Impressions)
	}
}

// TestSearchPerformanceHookNoDataIsData asserts a URL with no metrics reports
// has_data=false and non-nil empty Rows — honest absent data, never an error.
func TestSearchPerformanceHookNoDataIsData(t *testing.T) {
	ctx := context.Background()
	db, _ := openSeededStore(t)

	resp, err := searchPerformanceHook(db)(ctx, "https://a.test/nometrics", "")
	if err != nil {
		t.Fatalf("searchPerformanceHook no-data must not error: %v", err)
	}
	if resp.HasData {
		t.Fatalf("want HasData=false, got %+v", resp)
	}
	if resp.Rows == nil {
		t.Fatalf("Rows = nil, want non-nil empty slice")
	}
}

// TestSearchPerformanceHookBadSinceErrors asserts a malformed since string is a
// hook error (the daemon handler validates RFC3339 before calling, but the hook is
// defensive — an unparseable since is a caller fault, surfaced so the handler maps
// it to 400 / the bridge to an error).
func TestSearchPerformanceHookBadSinceErrors(t *testing.T) {
	ctx := context.Background()
	db, _ := openSeededStore(t)

	if _, err := searchPerformanceHook(db)(ctx, "https://a.test/p", "not-a-time"); err == nil {
		t.Fatalf("want an error for a malformed since, got nil")
	}
}
