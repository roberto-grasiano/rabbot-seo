package store

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// shiftDay renders a 2026 YYYY-MM-DD bucket for the search_performance_shift tests.
func shiftDay(month, d int) string {
	return time.Date(2026, time.Month(month), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// shiftMetric builds one (query,date) row for a single URL.
func shiftMetric(date, query string, impressions int64, pos float64) model.SearchMetric {
	return model.SearchMetric{URL: "https://ex.com/p", Query: query, Date: date, Impressions: impressions, Position: pos}
}

// ── unit tests for the relocated pure function (its home now lives in store) ──────

func TestSearchPerformanceShift_ImpressionDrop_Enriches(t *testing.T) {
	t.Parallel()
	// Change on 2026-06-10. Before (06-03..06-09) and after (06-11..06-17) windows
	// are both fully finalized as of now (06-21, lag 3 → final cutoff = 06-18).
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	var rows []model.SearchMetric
	for d := 3; d <= 9; d++ {
		rows = append(rows, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	for d := 11; d <= 17; d++ {
		rows = append(rows, shiftMetric(shiftDay(6, d), "widgets", 200, 9.0))
	}
	enr, ok := SearchPerformanceShift(rows, shiftDay(6, 10), now)
	if !ok {
		t.Fatalf("want an enrichment for a clear post-change drop")
	}
	if enr.Query != "widgets" {
		t.Errorf("primary query = %q, want widgets", enr.Query)
	}
	if enr.ImpressionsDelta >= 0 {
		t.Errorf("impressions delta = %d, want negative (a loss)", enr.ImpressionsDelta)
	}
	if enr.PositionDelta <= 0 { // position got worse (numerically larger)
		t.Errorf("position delta = %.2f, want positive (worse)", enr.PositionDelta)
	}
	if enr.AfterDays != 7 {
		t.Errorf("after days = %d, want 7 finalized", enr.AfterDays)
	}
	if enr.String() == "" {
		t.Errorf("enrichment must render a human string")
	}
}

// dataState=final discipline: when the only post-change data is within the partial
// lag (the latest ~3 days), there is NO finalized after-window → no enrichment.
func TestSearchPerformanceShift_OnlyPartialAfterData_NoEnrichment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // final cutoff = 06-18
	var rows []model.SearchMetric
	for d := 10; d <= 16; d++ {
		rows = append(rows, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	// change on 06-19: the only "after" days (06-20, 06-21) are partial.
	rows = append(rows, shiftMetric(shiftDay(6, 20), "widgets", 50, 9.0))
	if _, ok := SearchPerformanceShift(rows, shiftDay(6, 19), now); ok {
		t.Errorf("partial-only after-window must not enrich")
	}
}

// Not enough finalized after-days to be meaningful → no enrichment.
func TestSearchPerformanceShift_InsufficientAfterWindow_NoEnrichment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // final cutoff = 06-18
	var rows []model.SearchMetric
	for d := 3; d <= 9; d++ {
		rows = append(rows, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	// change on 06-17: only 06-18 is a finalized after-day (1 day < minimum 3).
	rows = append(rows, shiftMetric(shiftDay(6, 18), "widgets", 50, 9.0))
	if _, ok := SearchPerformanceShift(rows, shiftDay(6, 17), now); ok {
		t.Errorf("a single finalized after-day is insufficient; must not enrich")
	}
}

// no before-window data at all → cannot compute a delta → no enrichment.
func TestSearchPerformanceShift_NoBeforeData_NoEnrichment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	var rows []model.SearchMetric
	for d := 11; d <= 17; d++ {
		rows = append(rows, shiftMetric(shiftDay(6, d), "widgets", 200, 9.0))
	}
	if _, ok := SearchPerformanceShift(rows, shiftDay(6, 10), now); ok {
		t.Errorf("no before-window data must not enrich")
	}
}

// A malformed change date is a clean no-op (never an enrichment, never a panic).
func TestSearchPerformanceShift_BadChangeDate_NoEnrichment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if _, ok := SearchPerformanceShift(nil, "not-a-date", now); ok {
		t.Errorf("malformed change date must not enrich")
	}
}

// ── end-to-end: the enrichment surfaces from the production read path (BuildReport)

// seedShiftURL inserts a site + one URL + a single in-window change at changeAt, plus
// the supplied search_metrics for that URL, so BuildReport reaches the annotation
// through the real store. It returns the site id.
func seedShiftURL(t *testing.T, db *DB, urlStr string, changeAt time.Time, metrics []model.SearchMetric) int64 {
	t.Helper()
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://ex.com/")
	var urlID int64
	err := db.WriteTx(ctx, func(tx Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO urls (site_id, url, first_seen, next_check_at) VALUES (?,?,?,?)`,
			siteID, urlStr, changeAt.Add(-72*time.Hour), changeAt)
		if e != nil {
			return e
		}
		uid, e := res.LastInsertId()
		if e != nil {
			return e
		}
		urlID = uid
		sres, e := tx.ExecContext(ctx,
			`INSERT INTO snapshots (url_id, fetched_at) VALUES (?,?)`, urlID, changeAt)
		if e != nil {
			return e
		}
		snapID, e := sres.LastInsertId()
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx,
			`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (?,?,'title','substantive',?)`,
			urlID, snapID, changeAt)
		return e
	})
	if err != nil {
		t.Fatalf("seedShiftURL: %v", err)
	}
	// Point the metric rows at this site/url and persist them through the real writer.
	for i := range metrics {
		metrics[i].SiteID = siteID
		metrics[i].URL = urlStr
	}
	if err := db.SaveSearchMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}
	return siteID
}

// TestBuildReport_AnnotatesSearchShift is the headline end-to-end: a change record +
// seeded search_metrics spanning the change date → BuildReport surfaces the
// search_performance_shift annotation on the top-changed-URL row with correct deltas.
// This proves the enrichment is reached from the production read path, not just the
// isolated function.
func TestBuildReport_AnnotatesSearchShift(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()

	// now = 06-21 (final cutoff = 06-18); change detected 06-10.
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	const u = "https://ex.com/p"
	var metrics []model.SearchMetric
	for d := 3; d <= 9; d++ { // before: strong
		metrics = append(metrics, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	for d := 11; d <= 17; d++ { // after: collapsed (finalized)
		metrics = append(metrics, shiftMetric(shiftDay(6, d), "widgets", 200, 9.0))
	}
	seedShiftURL(t, db, u, changeAt, metrics)

	res, err := db.BuildReport(ctx, ReportParams{
		Since: now.Add(-21 * 24 * time.Hour),
		TopN:  10,
		Now:   now,
	})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(res.TopURLs) != 1 {
		t.Fatalf("want 1 top URL, got %d (%+v)", len(res.TopURLs), res.TopURLs)
	}
	row := res.TopURLs[0]
	if row.URL != u {
		t.Fatalf("top URL = %q, want %q", row.URL, u)
	}
	if row.SearchShift == nil {
		t.Fatalf("SearchShift not attached; the enrichment did not reach the read path")
	}
	if row.SearchShift.Query != "widgets" {
		t.Errorf("shift query = %q, want widgets", row.SearchShift.Query)
	}
	if row.SearchShift.ImpressionsDelta >= 0 {
		t.Errorf("impressions delta = %d, want negative", row.SearchShift.ImpressionsDelta)
	}
	if row.SearchShift.PositionDelta <= 0 {
		t.Errorf("position delta = %.2f, want positive (worse)", row.SearchShift.PositionDelta)
	}
}

// TestBuildReport_SearchShift_PartialData_NoAnnotation pins the no-fabrication
// guarantee: when the post-change data is only partial (not finalized), BuildReport
// attaches NOTHING — the changed-URL row is present, SearchShift is nil.
func TestBuildReport_SearchShift_PartialData_NoAnnotation(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // final cutoff = 06-18
	changeAt := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	const u = "https://ex.com/recent"
	var metrics []model.SearchMetric
	for d := 12; d <= 18; d++ { // before window only
		metrics = append(metrics, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	// The only after-day (06-20) is partial → no finalized after-window.
	metrics = append(metrics, shiftMetric(shiftDay(6, 20), "widgets", 50, 9.0))
	seedShiftURL(t, db, u, changeAt, metrics)

	res, err := db.BuildReport(ctx, ReportParams{
		Since: now.Add(-21 * 24 * time.Hour),
		TopN:  10,
		Now:   now,
	})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(res.TopURLs) != 1 {
		t.Fatalf("want the changed URL present, got %d rows", len(res.TopURLs))
	}
	if res.TopURLs[0].SearchShift != nil {
		t.Fatalf("partial-only data must attach NO enrichment, got %+v", res.TopURLs[0].SearchShift)
	}
}

// TestBuildReport_SearchShift_NoMetrics_NoAnnotation: a changed URL with no GSC search
// metrics at all is reported plainly — never a fabricated shift.
func TestBuildReport_SearchShift_NoMetrics_NoAnnotation(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	seedShiftURL(t, db, "https://ex.com/nometrics", changeAt, nil)

	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-21 * 24 * time.Hour), TopN: 10, Now: now})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(res.TopURLs) != 1 || res.TopURLs[0].SearchShift != nil {
		t.Fatalf("a URL with no metrics must carry no enrichment, got %+v", res.TopURLs)
	}
}

// TestBuildReport_SearchShift_AllSites_ResolvesSiteID proves the all-sites path (no
// site scope) resolves each URL's site id and still annotates — the batched
// url_id→site_id lookup feeds SearchMetricsForURL the right (site_id, url).
func TestBuildReport_SearchShift_AllSites_ResolvesSiteID(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	const u = "https://ex.com/p"
	var metrics []model.SearchMetric
	for d := 3; d <= 9; d++ {
		metrics = append(metrics, shiftMetric(shiftDay(6, d), "widgets", 1000, 4.0))
	}
	for d := 11; d <= 17; d++ {
		metrics = append(metrics, shiftMetric(shiftDay(6, d), "widgets", 200, 9.0))
	}
	seedShiftURL(t, db, u, changeAt, metrics)

	// SiteID nil → all-sites scope (exercises siteIDsForURLs).
	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-21 * 24 * time.Hour), TopN: 10, Now: now})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(res.TopURLs) != 1 || res.TopURLs[0].SearchShift == nil {
		t.Fatalf("all-sites report must still annotate via resolved site id, got %+v", res.TopURLs)
	}
}
