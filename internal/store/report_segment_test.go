package store

import (
	"context"
	"testing"
	"time"
)

// seedSegmentMemberships adds a "content" segment to site 1 (id 1) and makes the
// given url ids members, returning the segment id. It relies on the report
// fixture having already inserted the sites/urls.
func seedSegmentMemberships(t *testing.T, db *DB, siteID int64, urlIDs ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	var segID int64
	if err := db.WriteTx(ctx, func(tx Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO segments (site_id, name, match_rule) VALUES (?, 'content', '^/')`, siteID)
		if e != nil {
			return e
		}
		segID, e = res.LastInsertId()
		if e != nil {
			return e
		}
		for _, uid := range urlIDs {
			if _, e := tx.ExecContext(ctx,
				`INSERT INTO url_segments (url_id, segment_id) VALUES (?, ?)`, uid, segID); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seedSegmentMemberships: %v", err)
	}
	return segID
}

// TestBuildReport_SegmentFilter asserts a Segment scope restricts every
// sub-query (changes, issues, window counts, top URLs) to member URLs, and that
// each filtered total is <= its unfiltered counterpart.
func TestBuildReport_SegmentFilter(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedReportFixture(t, db, now)

	// Make only url 10 (site 1) a member of segment "content". url 11 (site 1)
	// and url 20 (site 2) stay out.
	seedSegmentMemberships(t, db, 1, 10)

	since := now.Add(-24 * time.Hour)
	unfiltered, err := db.BuildReport(ctx, ReportParams{Since: since, TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport(unfiltered): %v", err)
	}

	seg := "content"
	res, err := db.BuildReport(ctx, ReportParams{Since: since, TopN: 10, Segment: &seg})
	if err != nil {
		t.Fatalf("BuildReport(segment): %v", err)
	}

	// url 10 has 3 in-window changes (2 substantive, 1 cosmetic); only those count.
	if res.Changes != (ChangeSummary{Total: 3, Substantive: 2, Cosmetic: 1}) {
		t.Fatalf("segment Changes = %+v, want {3,2,1}", res.Changes)
	}
	// url 10 issues: open critical (title-missing in-window), open warning (meta-dupe out-of-window).
	// open now: critical 1 + warning 1 = 2; opened-in-window: 1 (the critical); closed-in-window: 0.
	wantIssues := IssueSummary{OpenTotal: 2, OpenCritical: 1, OpenWarning: 1, OpenedInWindow: 1, ClosedInWindow: 0}
	if res.Issues != wantIssues {
		t.Fatalf("segment Issues = %+v, want %+v", res.Issues, wantIssues)
	}
	// Top URLs: only url 10.
	if len(res.TopURLs) != 1 || res.TopURLs[0].URLID != 10 {
		t.Fatalf("segment TopURLs = %+v, want only url10", res.TopURLs)
	}

	// Filtered totals strictly <= unfiltered.
	if res.Changes.Total > unfiltered.Changes.Total {
		t.Fatalf("filtered changes %d > unfiltered %d", res.Changes.Total, unfiltered.Changes.Total)
	}
	if res.Issues.OpenTotal > unfiltered.Issues.OpenTotal {
		t.Fatalf("filtered open %d > unfiltered %d", res.Issues.OpenTotal, unfiltered.Issues.OpenTotal)
	}
	if res.Issues.OpenedInWindow > unfiltered.Issues.OpenedInWindow {
		t.Fatalf("filtered opened-in-window %d > unfiltered %d", res.Issues.OpenedInWindow, unfiltered.Issues.OpenedInWindow)
	}
	if len(res.TopURLs) > len(unfiltered.TopURLs) {
		t.Fatalf("filtered top urls %d > unfiltered %d", len(res.TopURLs), len(unfiltered.TopURLs))
	}
}

// TestBuildReport_SegmentFilter_SiteRollupsConsistent asserts that when a
// segment filter is applied to an all-sites report, the per-site rollup is
// scoped to the same member set (no leaking of non-member counts).
func TestBuildReport_SegmentFilter_SiteRollupsConsistent(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedReportFixture(t, db, now)
	// url 10 (site 1) only; site 2 has no members.
	seedSegmentMemberships(t, db, 1, 10)

	seg := "content"
	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-24 * time.Hour), TopN: 10, Segment: &seg})
	if err != nil {
		t.Fatalf("BuildReport(segment): %v", err)
	}
	// Only site 1 has member changes/issues; site 2 must be absent from the rollup.
	if len(res.Sites) != 1 || res.Sites[0].SiteID != 1 {
		t.Fatalf("segment rollup = %+v, want only site 1", res.Sites)
	}
	// site 1's member-only counts: 3 changes (url 10), 2 open issues (url 10).
	if res.Sites[0].Changes != 3 || res.Sites[0].OpenIssues != 2 {
		t.Fatalf("segment rollup site 1 = %+v, want {changes3, open2}", res.Sites[0])
	}
}

// TestBuildReport_SegmentFilter_UnknownEmpty asserts an unknown segment name
// degrades to all-zero data, never an error.
func TestBuildReport_SegmentFilter_UnknownEmpty(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedReportFixture(t, db, now)

	unknown := "does-not-exist"
	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-24 * time.Hour), TopN: 10, Segment: &unknown})
	if err != nil {
		t.Fatalf("BuildReport(unknown segment): %v", err)
	}
	if res.Changes.Total != 0 || res.Issues.OpenTotal != 0 || res.TopURLs != nil || res.Sites != nil {
		t.Fatalf("unknown segment report not zero-valued: %+v", res)
	}
}
