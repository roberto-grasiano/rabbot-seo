package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedReportFixture builds two sites with urls, changes, and issues spanning the
// window boundary. `now` is the reference; the window is [now-24h, now].
func seedReportFixture(t *testing.T, db *DB, now time.Time) (siteA, siteB int64) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := db.WriteTx(ctx, func(tx Tx) error {
			_, e := tx.ExecContext(ctx, q, args...)
			return e
		}); err != nil {
			t.Fatalf("seed exec: %v\nq=%s", err, q)
		}
	}
	in := now.Add(-1 * time.Hour)   // inside window
	out := now.Add(-48 * time.Hour) // outside window

	mustExec(`INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (1,'https://a.test','A',1,?,?)`, out, out)
	mustExec(`INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (2,'https://b.test','B',1,?,?)`, out, out)
	mustExec(`INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (10,1,'https://a.test/p1',?,?)`, out, now)
	mustExec(`INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (11,1,'https://a.test/p2',?,?)`, out, now)
	mustExec(`INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (20,2,'https://b.test/p1',?,?)`, out, now)
	// snapshots referenced by changes (FK + CASCADE); any row is fine.
	mustExec(`INSERT INTO snapshots (id, url_id, fetched_at) VALUES (100,10,?)`, in)
	mustExec(`INSERT INTO snapshots (id, url_id, fetched_at) VALUES (101,11,?)`, in)
	mustExec(`INSERT INTO snapshots (id, url_id, fetched_at) VALUES (102,20,?)`, in)

	// changes: url 10 -> 3 in-window (2 substantive, 1 cosmetic); url 11 -> 1 in-window; 1 out.
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'title','substantive',?)`, in)
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'meta_description','substantive',?)`, in)
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'h1','cosmetic',?)`, in)
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (11,101,'title','substantive',?)`, in)
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (11,101,'title','substantive',?)`, out)    // outside window
	mustExec(`INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (20,102,'canonical','substantive',?)`, in) // site B

	// issues: open critical (a), open warning (a), closed-in-window (resolved), ignored (excluded), opened-out-of-window-but-open.
	mustExec(`INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (10,'title-missing','open','critical',?,?)`, in, in)
	mustExec(`INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (10,'meta-dupe','open','warning',?,?)`, out, in)
	mustExec(`INSERT INTO issues (url_id, rule_id, status, severity, opened_at, closed_at, last_seen_at) VALUES (11,'canonical-bad','closed','warning',?,?,?)`, out, in, in)
	mustExec(`INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (11,'noindex','ignored','info',?,?)`, in, in)
	mustExec(`INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (20,'h1-missing','open','info',?,?)`, in, in)
	return 1, 2
}

func newReportTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "report.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedManyChangedURLs inserts n urls under one site, each with a single in-window
// change, to exercise the TopN LIMIT/guard.
func seedManyChangedURLs(t *testing.T, db *DB, now time.Time, n int) {
	t.Helper()
	ctx := context.Background()
	in := now.Add(-1 * time.Hour)
	err := db.WriteTx(ctx, func(tx Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (9,'https://many.test','M',1,?,?)`, in, in); e != nil {
			return e
		}
		for i := 0; i < n; i++ {
			uid := int64(900 + i)
			if _, e := tx.ExecContext(ctx, `INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (?,9,?,?,?)`, uid, fmt.Sprintf("https://many.test/p%d", i), in, now); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO snapshots (id, url_id, fetched_at) VALUES (?,?,?)`, uid, uid, in); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (?,?,'title','substantive',?)`, uid, uid, in); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seedManyChangedURLs: %v", err)
	}
}

func TestBuildReport_AllSites(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedReportFixture(t, db, now)

	res, err := db.BuildReport(context.Background(), ReportParams{Since: now.Add(-24 * time.Hour), TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	// changes in window: url10=3, url11=1, url20=1 => total 5; substantive 4, cosmetic 1.
	if res.Changes != (ChangeSummary{Total: 5, Substantive: 4, Cosmetic: 1}) {
		t.Fatalf("Changes = %+v, want {5,4,1}", res.Changes)
	}
	// open now: critical 1, warning 1, info 1 => total 3. opened-in-window: critical(a in) + ignored(in) + info(20 in) = 3. closed-in-window: 1.
	want := IssueSummary{OpenTotal: 3, OpenCritical: 1, OpenWarning: 1, OpenInfo: 1, OpenedInWindow: 3, ClosedInWindow: 1}
	if res.Issues != want {
		t.Fatalf("Issues = %+v, want %+v", res.Issues, want)
	}
	// top urls: url10(3) then url11(1)/url20(1) by last_changed desc then url_id asc.
	if len(res.TopURLs) != 3 || res.TopURLs[0].URLID != 10 || res.TopURLs[0].Count != 3 {
		t.Fatalf("TopURLs[0] = %+v, want url10 count 3", res.TopURLs)
	}
	if res.TopURLs[0].URL != "https://a.test/p1" {
		t.Fatalf("TopURLs[0].URL = %q", res.TopURLs[0].URL)
	}
	// per-site rollup (all-sites scope): A changes 4 (url10 3 + url11 1), B changes 1; A open 2, B open 1.
	if len(res.Sites) != 2 || res.Sites[0].SiteID != 1 || res.Sites[0].Changes != 4 || res.Sites[0].OpenIssues != 2 {
		t.Fatalf("Sites = %+v, want A first {changes4, open2}", res.Sites)
	}
	if res.Sites[1].SiteID != 2 || res.Sites[1].Changes != 1 || res.Sites[1].OpenIssues != 1 {
		t.Fatalf("Sites[1] = %+v, want B {changes1, open1}", res.Sites[1])
	}
}

func TestBuildReport_SiteScoped(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedReportFixture(t, db, now)
	siteB := int64(2)

	res, err := db.BuildReport(context.Background(), ReportParams{Since: now.Add(-24 * time.Hour), SiteID: &siteB, TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if res.Changes != (ChangeSummary{Total: 1, Substantive: 1, Cosmetic: 0}) {
		t.Fatalf("site B Changes = %+v, want {1,1,0}", res.Changes)
	}
	// Full IssueSummary for site B: locks the u.site_id = ? filter in issueWindowCount
	// for BOTH opened_at and closed_at — a dropped JOIN would leak all-sites counts
	// (OpenedInWindow 3, ClosedInWindow 1) and fail here.
	wantIssues := IssueSummary{OpenTotal: 1, OpenInfo: 1, OpenedInWindow: 1, ClosedInWindow: 0}
	if res.Issues != wantIssues {
		t.Fatalf("site B Issues = %+v, want %+v", res.Issues, wantIssues)
	}
	if res.Sites != nil {
		t.Fatalf("site-scoped report must omit per-site rollup, got %+v", res.Sites)
	}
	if len(res.TopURLs) != 1 || res.TopURLs[0].URLID != 20 {
		t.Fatalf("site B TopURLs = %+v, want only url20", res.TopURLs)
	}
}

// TestBuildReport_MonotonicDetectedAt guards the topChangedURLs timestamp
// round-trip against a detected_at that still carries a monotonic clock reading.
// A raw time.Now() (no .UTC()) serializes via time.Time.String() as
// "... +0000 UTC m=+0.000..." — the trailing " m=" segment breaks time.Parse with
// maxTimestampLayout unless topChangedURLs strips it. Production strips monotonic
// via .UTC() upstream, but this locks the parse so a future caller passing a raw
// clock can't silently break the report in prod while time.Date-seeded tests stay green.
func TestBuildReport_MonotonicDetectedAt(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	ctx := context.Background()
	// time.Now() (NOT .UTC()) keeps the monotonic reading; in-window relative to itself.
	mono := time.Now()
	in := mono.Add(-1 * time.Hour)
	if err := db.WriteTx(ctx, func(tx Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (1,'https://m.test','M',1,?,?)`, in, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (10,1,'https://m.test/p1',?,?)`, in, mono); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO snapshots (id, url_id, fetched_at) VALUES (100,10,?)`, in); e != nil {
			return e
		}
		// detected_at carries the monotonic reading.
		_, e := tx.ExecContext(ctx, `INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'title','substantive',?)`, mono)
		return e
	}); err != nil {
		t.Fatalf("seed monotonic: %v", err)
	}

	res, err := db.BuildReport(ctx, ReportParams{Since: in.Add(-1 * time.Hour), TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport with monotonic detected_at: %v", err)
	}
	if len(res.TopURLs) != 1 {
		t.Fatalf("TopURLs = %+v, want exactly url10", res.TopURLs)
	}
	if res.TopURLs[0].LastChanged.IsZero() {
		t.Fatalf("LastChanged is zero — monotonic timestamp parse failed silently")
	}
}

func TestBuildReport_EmptyAndTopNGuard(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// empty DB -> all zeros, nil slices.
	res, err := db.BuildReport(context.Background(), ReportParams{Since: now.Add(-24 * time.Hour), TopN: 0})
	if err != nil {
		t.Fatalf("BuildReport empty: %v", err)
	}
	if res.Changes.Total != 0 || res.Issues.OpenTotal != 0 || res.TopURLs != nil || res.Sites != nil {
		t.Fatalf("empty report not zero-valued: %+v", res)
	}

	// TopN<=0 must NOT become LIMIT -1 (unbounded). Seed 12 changed urls, default 10.
	seedManyChangedURLs(t, db, now, 12)
	res, err = db.BuildReport(context.Background(), ReportParams{Since: now.Add(-24 * time.Hour), TopN: 0})
	if err != nil {
		t.Fatalf("BuildReport many: %v", err)
	}
	if len(res.TopURLs) != 10 {
		t.Fatalf("TopN<=0 guard failed: got %d top urls, want default 10", len(res.TopURLs))
	}
}

// TestBuildReport_UnknownClassAndQuietSiteOmitted covers two defensive branches:
// (1) an unrecognised change_class is excluded from the totals so the documented
// invariant Total == Substantive + Cosmetic always holds; (2) a site with no
// in-window changes and no open issues is omitted from the per-site rollup
// (active-or-problematic membership).
func TestBuildReport_UnknownClassAndQuietSiteOmitted(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	ctx := context.Background()
	if err := db.WriteTx(ctx, func(tx Tx) error {
		// active site 1 + a quiet, healthy site 2 (no changes, no open issues).
		if _, e := tx.ExecContext(ctx, `INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (1,'https://u.test','U',1,?,?)`, in, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (2,'https://quiet.test','Q',1,?,?)`, in, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (10,1,'https://u.test/p',?,?)`, in, now); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (20,2,'https://quiet.test/p',?,?)`, in, now); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO snapshots (id, url_id, fetched_at) VALUES (100,10,?)`, in); e != nil {
			return e
		}
		// site 1: one substantive, one cosmetic, one UNKNOWN class — all in window.
		if _, e := tx.ExecContext(ctx, `INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'title','substantive',?)`, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'h1','cosmetic',?)`, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO changes (url_id, snapshot_id, field, change_class, detected_at) VALUES (10,100,'weird','mystery',?)`, in); e != nil {
			return e
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-24 * time.Hour), TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	// (1) unknown class excluded; invariant holds (sub 1 + cos 1 = total 2, NOT 3).
	if res.Changes != (ChangeSummary{Total: 2, Substantive: 1, Cosmetic: 1}) {
		t.Fatalf("Changes = %+v, want {2,1,1} (unknown class excluded, Total==Sub+Cos)", res.Changes)
	}
	// (2) the quiet+healthy site 2 is omitted; only the active site 1 appears.
	if len(res.Sites) != 1 || res.Sites[0].SiteID != 1 {
		t.Fatalf("Sites = %+v, want only active site 1 (quiet+healthy site omitted)", res.Sites)
	}
}

// TestBuildReport_UnknownSeverityExcludedFromOpenTotal is the issue-side twin of
// the change_class invariant test: an open issue with an unrecognised severity
// must be excluded from OpenTotal so OpenTotal == OpenCritical + OpenWarning +
// OpenInfo always holds.
func TestBuildReport_UnknownSeverityExcludedFromOpenTotal(t *testing.T) {
	t.Parallel()
	db := newReportTestDB(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	ctx := context.Background()
	if err := db.WriteTx(ctx, func(tx Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO sites (id, base_url, name, enabled, created_at, updated_at) VALUES (1,'https://s.test','S',1,?,?)`, in, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO urls (id, site_id, url, first_seen, next_check_at) VALUES (10,1,'https://s.test/p',?,?)`, in, now); e != nil {
			return e
		}
		// one open critical (known) + one open with an unrecognised severity.
		if _, e := tx.ExecContext(ctx, `INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (10,'r-known','open','critical',?,?)`, in, in); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO issues (url_id, rule_id, status, severity, opened_at, last_seen_at) VALUES (10,'r-weird','open','bizarre',?,?)`, in, in); e != nil {
			return e
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := db.BuildReport(ctx, ReportParams{Since: now.Add(-24 * time.Hour), TopN: 10})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if res.Issues.OpenCritical != 1 {
		t.Fatalf("OpenCritical = %d, want 1", res.Issues.OpenCritical)
	}
	if res.Issues.OpenTotal != res.Issues.OpenCritical+res.Issues.OpenWarning+res.Issues.OpenInfo {
		t.Fatalf("invariant broken: OpenTotal=%d != Critical+Warning+Info=%d",
			res.Issues.OpenTotal, res.Issues.OpenCritical+res.Issues.OpenWarning+res.Issues.OpenInfo)
	}
	if res.Issues.OpenTotal != 1 {
		t.Fatalf("OpenTotal = %d, want 1 (unknown severity excluded)", res.Issues.OpenTotal)
	}
}
