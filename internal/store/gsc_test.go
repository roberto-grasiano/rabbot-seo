package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// gscTestSite inserts a site and returns its id, so GSC rows satisfy the
// site_id FK (PRAGMA foreign_keys = ON).
func gscTestSite(t *testing.T, db *DB, base string) int64 {
	t.Helper()
	id, err := db.AddSite(context.Background(), model.Site{BaseURL: base, Name: "ex", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite(%s): %v", base, err)
	}
	return id
}

// TestSaveSearchMetricsRoundTrip writes a batch and reads it back through
// SearchMetricsForURL, asserting every column survives the persisted encoding
// (the PERSISTED-ENCODING lesson: assert EXACTLY what is written reads back, via
// the real write+read path). A no-row date filter and ordering are covered too.
func TestSaveSearchMetricsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	rows := []model.SearchMetric{
		{SiteID: siteID, URL: "https://example.com/a", Query: "kraken", Date: "2026-06-10", Clicks: 5, Impressions: 100, CTR: 0.05, Position: 3.2},
		{SiteID: siteID, URL: "https://example.com/a", Query: "rabbot", Date: "2026-06-10", Clicks: 1, Impressions: 40, CTR: 0.025, Position: 8.0},
		{SiteID: siteID, URL: "https://example.com/a", Query: "kraken", Date: "2026-06-11", Clicks: 7, Impressions: 120, CTR: 0.0583, Position: 2.9},
	}
	if err := db.SaveSearchMetrics(ctx, rows); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}

	got, err := db.SearchMetricsForURL(ctx, siteID, "https://example.com/a", time.Time{})
	if err != nil {
		t.Fatalf("SearchMetricsForURL: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d metrics, want 3", len(got))
	}

	// Find the (kraken, 2026-06-10) row and assert every field round-trips.
	var found *model.SearchMetric
	for i := range got {
		if got[i].Query == "kraken" && got[i].Date == "2026-06-10" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("(kraken,2026-06-10) row not found in %+v", got)
	}
	if found.SiteID != siteID || found.URL != "https://example.com/a" ||
		found.Clicks != 5 || found.Impressions != 100 || found.CTR != 0.05 || found.Position != 3.2 {
		t.Errorf("round-trip mismatch: got %+v", *found)
	}
	if found.ID == 0 {
		t.Errorf("ID not populated on read, got 0")
	}
}

// TestSaveSearchMetricsUpsertIdempotent re-pulls the SAME (site,url,query,date)
// grain with updated metrics and asserts the row is REPLACED, not duplicated —
// the UNIQUE(site_id,url,query,date) + ON CONFLICT DO UPDATE backfill contract.
func TestSaveSearchMetricsUpsertIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	first := []model.SearchMetric{
		{SiteID: siteID, URL: "https://example.com/p", Query: "q", Date: "2026-06-10", Clicks: 1, Impressions: 10, CTR: 0.1, Position: 9},
	}
	if err := db.SaveSearchMetrics(ctx, first); err != nil {
		t.Fatalf("SaveSearchMetrics first: %v", err)
	}
	// Re-pull the same grain with corrected (finalized) metrics.
	second := []model.SearchMetric{
		{SiteID: siteID, URL: "https://example.com/p", Query: "q", Date: "2026-06-10", Clicks: 4, Impressions: 50, CTR: 0.08, Position: 4.5},
	}
	if err := db.SaveSearchMetrics(ctx, second); err != nil {
		t.Fatalf("SaveSearchMetrics second: %v", err)
	}

	got, err := db.SearchMetricsForURL(ctx, siteID, "https://example.com/p", time.Time{})
	if err != nil {
		t.Fatalf("SearchMetricsForURL: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows after re-pull, want 1 (upsert, not append)", len(got))
	}
	if got[0].Clicks != 4 || got[0].Impressions != 50 || got[0].CTR != 0.08 || got[0].Position != 4.5 {
		t.Errorf("upsert did not refresh metrics, got %+v", got[0])
	}
}

// TestSaveSearchMetricsCanonicalizesURL asserts the write boundary applies the
// shared canonicalizer, so a divergent-but-equivalent input URL is stored under
// (and reads back via) the canonical keyspace urls.url / link_edges.to_url use —
// the join W2's signals depend on. A no-slash host folds to "/".
func TestSaveSearchMetricsCanonicalizesURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	// Mixed-case host + default port — canonicalURL lowercases host and drops :443.
	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteID, URL: "https://EXAMPLE.com:443/a", Query: "q", Date: "2026-06-10", Clicks: 2, Impressions: 9},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}

	// Reading with the canonical form returns the row.
	got, err := db.SearchMetricsForURL(ctx, siteID, "https://example.com/a", time.Time{})
	if err != nil {
		t.Fatalf("SearchMetricsForURL: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("canonical read got %d rows, want 1 (write/read keyspaces must match)", len(got))
	}
	if got[0].URL != "https://example.com/a" {
		t.Errorf("stored URL = %q, want canonical %q", got[0].URL, "https://example.com/a")
	}
}

// TestSearchMetricsForURLSinceFilter asserts the date-window read: only rows with
// date >= since (lexical 'YYYY-MM-DD' compare) come back, newest grouping aside.
func TestSearchMetricsForURLSinceFilter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteID, URL: "https://example.com/a", Query: "q", Date: "2026-06-08", Clicks: 1, Impressions: 1},
		{SiteID: siteID, URL: "https://example.com/a", Query: "q", Date: "2026-06-10", Clicks: 2, Impressions: 2},
		{SiteID: siteID, URL: "https://example.com/a", Query: "q", Date: "2026-06-12", Clicks: 3, Impressions: 3},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}

	got, err := db.SearchMetricsForURL(ctx, siteID, "https://example.com/a", mustDay(t, "2026-06-10"))
	if err != nil {
		t.Fatalf("SearchMetricsForURL since: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("since-filter got %d rows, want 2 (>= 2026-06-10)", len(got))
	}
	for _, m := range got {
		if m.Date < "2026-06-10" {
			t.Errorf("row with date %q leaked past the since filter", m.Date)
		}
	}
}

// TestSaveSearchMetricsEmptyNoop asserts a zero-length batch is a clean no-op
// (no tx churn, no error) — the puller may legitimately have nothing to store.
func TestSaveSearchMetricsEmptyNoop(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.SaveSearchMetrics(ctx, nil); err != nil {
		t.Fatalf("SaveSearchMetrics(nil): %v", err)
	}
	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{}); err != nil {
		t.Fatalf("SaveSearchMetrics([]): %v", err)
	}
}

// TestUpsertURLIndexStatusRoundTrip writes an inspection and reads it back through
// LatestURLIndexStatus, asserting every verdict field + both timestamps survive
// the persisted encoding (sibling-scan guard: a shifted column would corrupt an
// adjacent field). Times read back UTC.
func TestUpsertURLIndexStatusRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	lastCrawl := time.Date(2026, 6, 9, 14, 30, 0, 0, time.UTC)
	inspectedAt := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	want := model.URLIndexStatus{
		SiteID:          siteID,
		URL:             "https://example.com/a",
		InspectedAt:     inspectedAt,
		Verdict:         "PASS",
		CoverageState:   "Indexed, not submitted in sitemap",
		IndexingState:   "INDEXING_ALLOWED",
		RobotsTxtState:  "ALLOWED",
		PageFetchState:  "SUCCESSFUL",
		GoogleCanonical: "https://example.com/a",
		UserCanonical:   "https://example.com/a",
		CrawledAs:       "MOBILE",
		LastCrawlTime:   &lastCrawl,
	}
	if err := db.UpsertURLIndexStatus(ctx, want); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}

	got, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://example.com/a")
	if err != nil {
		t.Fatalf("LatestURLIndexStatus: %v", err)
	}
	if !ok {
		t.Fatalf("LatestURLIndexStatus ok = false, want true")
	}
	if got.Verdict != want.Verdict || got.CoverageState != want.CoverageState ||
		got.IndexingState != want.IndexingState || got.RobotsTxtState != want.RobotsTxtState ||
		got.PageFetchState != want.PageFetchState || got.GoogleCanonical != want.GoogleCanonical ||
		got.UserCanonical != want.UserCanonical || got.CrawledAs != want.CrawledAs {
		t.Errorf("verdict fields round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got.LastCrawlTime == nil || !got.LastCrawlTime.Equal(lastCrawl) {
		t.Errorf("LastCrawlTime round-trip = %v, want %v", got.LastCrawlTime, lastCrawl)
	}
	if !got.LastCrawlTime.Equal(got.LastCrawlTime.UTC()) {
		t.Errorf("LastCrawlTime not UTC on read: %v", got.LastCrawlTime)
	}
	if !got.InspectedAt.Equal(inspectedAt) {
		t.Errorf("InspectedAt round-trip = %v, want %v", got.InspectedAt, inspectedAt)
	}
}

// TestUpsertURLIndexStatusReplaces asserts the (site_id,url) UNIQUE upsert keeps
// ONE latest row per URL: a second inspection overwrites the first, never appends.
func TestUpsertURLIndexStatusReplaces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteID, URL: "https://example.com/p", InspectedAt: mustDay(t, "2026-06-10"),
		Verdict: "PASS", CoverageState: "Submitted and indexed",
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteID, URL: "https://example.com/p", InspectedAt: mustDay(t, "2026-06-11"),
		Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed",
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://example.com/p")
	if err != nil || !ok {
		t.Fatalf("LatestURLIndexStatus ok=%v err=%v", ok, err)
	}
	if got.Verdict != "NEUTRAL" || got.CoverageState != "Crawled - currently not indexed" {
		t.Errorf("upsert did not replace: got %+v", got)
	}

	// Exactly one row for the URL (no append).
	var n int
	if err := db.Read().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM url_index_status WHERE site_id = ? AND url = ?",
		siteID, "https://example.com/p").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (upsert, not append)", n)
	}
}

// TestUpsertURLIndexStatusNullLastCrawl asserts a nil LastCrawlTime persists as
// SQL NULL and reads back nil (Google reporting no last crawl), not a zero time.
func TestUpsertURLIndexStatusNullLastCrawl(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteID, URL: "https://example.com/x", InspectedAt: mustDay(t, "2026-06-10"),
		Verdict: "FAIL", LastCrawlTime: nil,
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}

	got, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://example.com/x")
	if err != nil || !ok {
		t.Fatalf("LatestURLIndexStatus ok=%v err=%v", ok, err)
	}
	if got.LastCrawlTime != nil {
		t.Errorf("LastCrawlTime = %v, want nil (Google reported no last crawl)", got.LastCrawlTime)
	}
}

// TestUpsertURLIndexStatusCanonicalizesURL asserts the write+read boundary shares
// the canonical keyspace (W2 joins GSC status to urls.url by exact string).
func TestUpsertURLIndexStatusCanonicalizesURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteID, URL: "https://EXAMPLE.com/a", InspectedAt: mustDay(t, "2026-06-10"), Verdict: "PASS",
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}
	got, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://example.com/a")
	if err != nil {
		t.Fatalf("LatestURLIndexStatus: %v", err)
	}
	if !ok {
		t.Fatalf("canonical read ok=false; write/read keyspaces must match")
	}
	if got.URL != "https://example.com/a" {
		t.Errorf("stored URL = %q, want canonical %q", got.URL, "https://example.com/a")
	}
}

// TestLatestURLIndexStatusMissing asserts the not-found arm mirrors
// LatestFileSnapshot: (zero, false, nil), never an error, for an unknown URL.
func TestLatestURLIndexStatusMissing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	got, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://example.com/never")
	if err != nil {
		t.Fatalf("LatestURLIndexStatus unknown URL erred: %v", err)
	}
	if ok {
		t.Errorf("ok = true for unknown URL, want false")
	}
	if got.ID != 0 {
		t.Errorf("got non-zero row %+v for unknown URL", got)
	}
}

// TestGSCSiteScoped asserts both reads are site-scoped: one site's GSC rows never
// leak into another site's reads (the multi-tenant invariant).
func TestGSCSiteScoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteA := gscTestSite(t, db, "https://a.com/")
	siteB := gscTestSite(t, db, "https://b.com/")

	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteA, URL: "https://a.com/p", Query: "q", Date: "2026-06-10", Clicks: 1, Impressions: 1},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics A: %v", err)
	}
	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteA, URL: "https://a.com/p", InspectedAt: mustDay(t, "2026-06-10"), Verdict: "PASS",
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus A: %v", err)
	}

	// Site B sees nothing for the same path string.
	metrics, err := db.SearchMetricsForURL(ctx, siteB, "https://a.com/p", time.Time{})
	if err != nil {
		t.Fatalf("SearchMetricsForURL B: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("site B leaked %d of site A's metrics", len(metrics))
	}
	if _, ok, err := db.LatestURLIndexStatus(ctx, siteB, "https://a.com/p"); err != nil || ok {
		t.Errorf("site B leaked site A's index status (ok=%v err=%v)", ok, err)
	}
}

// TestGSCSiteCascadeDelete asserts the ON DELETE CASCADE: removing a site drops
// its GSC rows (PRAGMA foreign_keys = ON), so orphaned GSC data cannot accumulate.
func TestGSCSiteCascadeDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := gscTestSite(t, db, "https://example.com/")

	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteID, URL: "https://example.com/p", Query: "q", Date: "2026-06-10", Clicks: 1, Impressions: 1},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}
	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: siteID, URL: "https://example.com/p", InspectedAt: mustDay(t, "2026-06-10"), Verdict: "PASS",
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}

	if err := db.WriteTx(ctx, func(tx Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", siteID)
		return e
	}); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	var sm, uis int
	if err := db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM search_metrics WHERE site_id = ?", siteID).Scan(&sm); err != nil {
		t.Fatalf("count search_metrics: %v", err)
	}
	if err := db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM url_index_status WHERE site_id = ?", siteID).Scan(&uis); err != nil {
		t.Fatalf("count url_index_status: %v", err)
	}
	if sm != 0 || uis != 0 {
		t.Errorf("cascade delete left rows: search_metrics=%d url_index_status=%d, want 0/0", sm, uis)
	}
}

// mustDay parses a 'YYYY-MM-DD' day to a UTC midnight time.Time for test fixtures.
func mustDay(t *testing.T, day string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatalf("parse day %q: %v", day, err)
	}
	return parsed.UTC()
}

// openClosedDB opens a store and immediately closes it, so subsequent repo calls hit
// the DB-error branches (exec/query against a closed handle).
func openClosedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if cerr := db.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	return db
}

// TestGSCStore_WriteErrorsSurfaced covers the exec-error arms of the two write
// methods: a closed store makes the upsert transaction fail with a wrapped error
// naming the site/url (never panicking).
func TestGSCStore_WriteErrorsSurfaced(t *testing.T) {
	db := openClosedDB(t)
	ctx := context.Background()

	smErr := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: 1, URL: "https://ex.com/p", Query: "q", Date: "2026-06-10", Clicks: 1, Impressions: 1},
	})
	if smErr == nil {
		t.Error("SaveSearchMetrics against a closed store must error")
	}
	uisErr := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID: 1, URL: "https://ex.com/p", InspectedAt: mustDay(t, "2026-06-10"), Verdict: "PASS",
	})
	if uisErr == nil {
		t.Error("UpsertURLIndexStatus against a closed store must error")
	}
}

// TestGSCStore_ReadErrorsSurfaced covers the query-error arms of the two read methods
// against a closed store.
func TestGSCStore_ReadErrorsSurfaced(t *testing.T) {
	db := openClosedDB(t)
	ctx := context.Background()

	if _, err := db.SearchMetricsForURL(ctx, 1, "https://ex.com/p", time.Time{}); err == nil {
		t.Error("SearchMetricsForURL against a closed store must error")
	}
	if _, _, err := db.LatestURLIndexStatus(ctx, 1, "https://ex.com/p"); err == nil {
		t.Error("LatestURLIndexStatus against a closed store must error")
	}
}
