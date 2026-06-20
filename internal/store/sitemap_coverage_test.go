package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// seedSitemapURL inserts a URL with explicit in_sitemap / last_checked control so
// the reconcile + coverage tests can assert exact pre/post state.
func seedSitemapURL(t *testing.T, db *DB, siteID int64, url string, inSitemap bool, lastChecked *time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: url, FirstSeen: now, LastChecked: lastChecked,
		NextCheckAt: now.Add(10 * time.Minute), Interval: 600, Importance: 0.5, Depth: 1,
		InSitemap: inSitemap, StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	})
	if err != nil {
		t.Fatalf("seedURL %q: %v", url, err)
	}
	return id
}

// urlSchedState reads back the scheduling columns + in_sitemap for invariant checks.
func urlSchedState(t *testing.T, db *DB, id int64) (inSitemap bool, nextCheckAt time.Time, interval int64, importance float64) {
	t.Helper()
	err := db.Read().QueryRowContext(context.Background(),
		`SELECT in_sitemap, next_check_at, interval, importance FROM urls WHERE id = ?`, id).
		Scan(&inSitemap, &nextCheckAt, &interval, &importance)
	if err != nil {
		t.Fatalf("urlSchedState %d: %v", id, err)
	}
	return
}

// TestReconcileSitemapMembership is acceptance criterion 6: a link-discovered URL
// present in the freshly collected set flips in_sitemap 0→1; an absent one 1→0;
// next_check_at / interval / importance are untouched by the reconcile.
func TestReconcileSitemapMembership(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	crawled := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	// present: link-discovered (in_sitemap=0), will be in the set → flips to 1.
	present := seedSitemapURL(t, db, siteID, "https://example.com/present", false, &crawled)
	// absent: currently flagged in_sitemap=1, NOT in the set → flips to 0.
	absent := seedSitemapURL(t, db, siteID, "https://example.com/absent", true, &crawled)
	// untouched-by-set: a URL in the set already at 1 stays 1.
	stay := seedSitemapURL(t, db, siteID, "https://example.com/stay", true, &crawled)

	// capture scheduling columns before reconcile
	_, presentNext, presentInterval, presentImp := urlSchedState(t, db, present)
	_, absentNext, absentInterval, absentImp := urlSchedState(t, db, absent)

	locs := []string{"https://example.com/present", "https://example.com/stay"}
	if err := db.ReconcileSitemapMembership(ctx, siteID, locs, false); err != nil {
		t.Fatalf("ReconcileSitemapMembership: %v", err)
	}

	if in, _, _, _ := urlSchedState(t, db, present); !in {
		t.Errorf("present: in_sitemap = false, want true (0→1 flip)")
	}
	if in, _, _, _ := urlSchedState(t, db, absent); in {
		t.Errorf("absent: in_sitemap = true, want false (1→0 flip)")
	}
	if in, _, _, _ := urlSchedState(t, db, stay); !in {
		t.Errorf("stay: in_sitemap = false, want true (unchanged)")
	}

	// Scheduling columns must be untouched by the reconcile.
	if _, n, iv, imp := urlSchedState(t, db, present); !n.Equal(presentNext) || iv != presentInterval || imp != presentImp {
		t.Errorf("present scheduling columns changed: next=%v(want %v) interval=%d(want %d) importance=%v(want %v)",
			n, presentNext, iv, presentInterval, imp, presentImp)
	}
	if _, n, iv, imp := urlSchedState(t, db, absent); !n.Equal(absentNext) || iv != absentInterval || imp != absentImp {
		t.Errorf("absent scheduling columns changed: next=%v(want %v) interval=%d(want %d) importance=%v(want %v)",
			n, absentNext, iv, absentInterval, imp, absentImp)
	}
}

// TestReconcileSitemapMembershipAdditiveOnly is the safety half of criterion 5 at
// the store layer: with additiveOnly=true (an incomplete collection), present
// rows are still set to 1, but absent rows are NEVER flipped 1→0.
func TestReconcileSitemapMembershipAdditiveOnly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	crawled := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	add := seedSitemapURL(t, db, siteID, "https://example.com/add", false, &crawled)
	keep := seedSitemapURL(t, db, siteID, "https://example.com/keep", true, &crawled)

	// Only /add is in the (partial) set; /keep is absent but must NOT flip off.
	if err := db.ReconcileSitemapMembership(ctx, siteID,
		[]string{"https://example.com/add"}, true); err != nil {
		t.Fatalf("ReconcileSitemapMembership(additiveOnly): %v", err)
	}

	if in, _, _, _ := urlSchedState(t, db, add); !in {
		t.Errorf("add: in_sitemap = false, want true (additions still admitted)")
	}
	if in, _, _, _ := urlSchedState(t, db, keep); !in {
		t.Errorf("keep: in_sitemap = false, want true (additive-only must not flip 1→0)")
	}
}

// TestReconcileSitemapMembershipChunking proves the chunked IN-list path handles a
// set larger than SQLite's bind-variable ceiling without error and reconciles
// every member.
func TestReconcileSitemapMembershipChunking(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	const n = 2500 // > sqliteMaxBindVars chunk size to force multiple IN lists
	locs := make([]string, 0, n)
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		u := "https://example.com/p/" + strconv.Itoa(i)
		locs = append(locs, u)
		ids = append(ids, seedSitemapURL(t, db, siteID, u, false, nil))
	}
	// one extra row NOT in the set, pre-flagged → must flip 1→0.
	off := seedSitemapURL(t, db, siteID, "https://example.com/off", true, nil)

	if err := db.ReconcileSitemapMembership(ctx, siteID, locs, false); err != nil {
		t.Fatalf("ReconcileSitemapMembership(chunked): %v", err)
	}
	for _, id := range ids {
		if in, _, _, _ := urlSchedState(t, db, id); !in {
			t.Fatalf("chunked member id=%d in_sitemap = false, want true", id)
		}
	}
	if in, _, _, _ := urlSchedState(t, db, off); in {
		t.Errorf("off: in_sitemap = true, want false (flipped by full reconcile)")
	}
}

// TestReproUnadmittedTransientPartial probes the deep-sweep finding:
// "unadmitted = len(locs) - InSitemapTotal on a partial/empty read yields a
// spurious positive that feeds a phantom drift alert." It drives the exact store
// calls RefreshSitemap makes (reconcile additive-only, then SitemapLiveCounts) for
// the realistic two-pass transient-partial scenario and applies the source's
// unadmitted formula to see whether a transient partial read manufactures
// unadmitted > 0 against a prior unadmitted == 0.
func TestReproUnadmittedTransientPartial(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// unadmitted as RefreshSitemap derives it (sidetimers_sitemap.go:95-98).
	unadmitted := func(locs []string, inSitemapTotal int) int {
		u := len(locs) - inSitemapTotal
		if u < 0 {
			u = 0
		}
		return u
	}

	// --- Pass 1: a COMPLETE read of the full declared set {a,b,c}. All three are
	// admitted into the inventory (CollectAndSeed seeds them) and reconciled to
	// in_sitemap=1, so InSitemapTotal == |locs| and unadmitted == 0. ---
	full := []string{
		"https://example.com/a", "https://example.com/b", "https://example.com/c",
	}
	for _, u := range full {
		// CollectAndSeed admits each declared loc (in_sitemap=1).
		seedSitemapURL(t, db, siteID, u, true, nil)
	}
	if err := db.ReconcileSitemapMembership(ctx, siteID, full, false); err != nil {
		t.Fatalf("pass1 reconcile: %v", err)
	}
	c1, err := db.SitemapLiveCounts(ctx, siteID)
	if err != nil {
		t.Fatalf("pass1 live counts: %v", err)
	}
	un1 := unadmitted(full, c1.InSitemapTotal)
	if un1 != 0 {
		t.Fatalf("pass1 baseline unadmitted = %d, want 0 (full set fully admitted)", un1)
	}

	// --- Pass 2: a TRANSIENT PARTIAL read returns only {a} (b,c unread this pass);
	// Incomplete=true so the reconcile is additive-only. /a was already admitted in
	// pass 1, so no new admission happens. len(locs)==1 now, but InSitemapTotal is
	// still 3 (additive-only never flips b,c off). ---
	partial := []string{"https://example.com/a"}
	if err := db.ReconcileSitemapMembership(ctx, siteID, partial, true); err != nil {
		t.Fatalf("pass2 reconcile (additiveOnly): %v", err)
	}
	c2, err := db.SitemapLiveCounts(ctx, siteID)
	if err != nil {
		t.Fatalf("pass2 live counts: %v", err)
	}
	un2 := unadmitted(partial, c2.InSitemapTotal)

	// The finding claims the partial read manufactures unadmitted=1. If the floor
	// + additive-only reconcile work, the partial read yields len=1, total=3 →
	// 1-3 = -2 → floored to 0, so NO phantom unadmitted growth.
	if un2 != 0 {
		t.Errorf("FINDING CONFIRMED: transient partial read manufactured unadmitted=%d "+
			"(len(locs)=%d InSitemapTotal=%d) — phantom drift", un2, len(partial), c2.InSitemapTotal)
	} else {
		t.Logf("partial read: len(locs)=%d InSitemapTotal=%d → unadmitted=%d (no phantom drift)",
			len(partial), c2.InSitemapTotal, un2)
	}

	// --- Pass 3 (true page-cap reject): a declared loc that is NEVER admitted (SSRF
	// reject / page-cap exhaustion in CollectAndSeed). It is in locs but absent from
	// the urls table, so the reconcile's "set present" UPDATE can't flag it. THIS is
	// the legitimate unadmitted>0 the field is meant to surface — not a partial-read
	// artifact. ---
	capExhausted := append([]string{}, full...)
	capExhausted = append(capExhausted, "https://example.com/never-admitted")
	if err := db.ReconcileSitemapMembership(ctx, siteID, capExhausted, false); err != nil {
		t.Fatalf("pass3 reconcile: %v", err)
	}
	c3, err := db.SitemapLiveCounts(ctx, siteID)
	if err != nil {
		t.Fatalf("pass3 live counts: %v", err)
	}
	un3 := unadmitted(capExhausted, c3.InSitemapTotal)
	t.Logf("genuine cap-reject: len(locs)=%d InSitemapTotal=%d → unadmitted=%d (legit)",
		len(capExhausted), c3.InSitemapTotal, un3)
	if un3 != 1 {
		t.Errorf("genuine unadmitted derivation broken: got %d, want 1", un3)
	}
}

// TestSitemapCoverage is acceptance criterion 8: SitemapCoverage on a seeded
// fixture returns exact counts and ≤10 samples per bucket; a no-sitemap site
// returns a zero-value result with HasSitemap=false.
func TestSitemapCoverage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	crawled := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	// sitemapped_uncrawled: in_sitemap=1 AND last_checked IS NULL → 3 rows.
	for i := 0; i < 3; i++ {
		seedSitemapURL(t, db, siteID, "https://example.com/uncrawled/"+strconv.Itoa(i), true, nil)
	}
	// crawled_not_in_sitemap: in_sitemap=0 AND last_checked IS NOT NULL → 4 rows.
	for i := 0; i < 4; i++ {
		seedSitemapURL(t, db, siteID, "https://example.com/orphan/"+strconv.Itoa(i), false, &crawled)
	}
	// noise that belongs to neither bucket: sitemapped AND crawled.
	seedSitemapURL(t, db, siteID, "https://example.com/healthy", true, &crawled)

	// Persist a sitemap snapshot whose coverage block carries sitemapped_unadmitted.
	doc := `{"v":1,"count":3,"truncated":false,"incomplete":false,` +
		`"urls":["https://example.com/uncrawled/0","https://example.com/uncrawled/1","https://example.com/uncrawled/2"],` +
		`"urls_capped":false,"coverage":{"sitemapped_uncrawled":3,"sitemapped_unadmitted":7,"crawled_not_in_sitemap":4}}`
	if _, err := db.SaveFileSnapshot(ctx, model.FileSnapshot{
		SiteID: siteID, Kind: model.FileKindSitemap, FetchedAt: time.Now().UTC(),
		ContentSHA256: "hash1", ParsedEntries: doc, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("SaveFileSnapshot: %v", err)
	}

	cov, err := db.SitemapCoverage(ctx, siteID)
	if err != nil {
		t.Fatalf("SitemapCoverage: %v", err)
	}

	if !cov.HasSitemap {
		t.Errorf("HasSitemap = false, want true")
	}
	if cov.SitemappedUncrawled != 3 {
		t.Errorf("SitemappedUncrawled = %d, want 3", cov.SitemappedUncrawled)
	}
	if cov.CrawledNotInSitemap != 4 {
		t.Errorf("CrawledNotInSitemap = %d, want 4", cov.CrawledNotInSitemap)
	}
	if cov.SitemappedUnadmitted != 7 {
		t.Errorf("SitemappedUnadmitted = %d, want 7 (from snapshot coverage block)", cov.SitemappedUnadmitted)
	}
	if cov.SeedStatus != 200 {
		t.Errorf("SeedStatus = %d, want 200", cov.SeedStatus)
	}
	if len(cov.SampleUncrawled) != 3 {
		t.Errorf("SampleUncrawled len = %d, want 3", len(cov.SampleUncrawled))
	}
	if len(cov.SampleNotInSitemap) != 4 {
		t.Errorf("SampleNotInSitemap len = %d, want 4", len(cov.SampleNotInSitemap))
	}

	// no-sitemap site → zero-value, HasSitemap=false.
	otherID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://other.example", Name: "Other", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite other: %v", err)
	}
	empty, err := db.SitemapCoverage(ctx, otherID)
	if err != nil {
		t.Fatalf("SitemapCoverage(no sitemap): %v", err)
	}
	if empty.HasSitemap {
		t.Errorf("no-sitemap site HasSitemap = true, want false")
	}
	if empty.SitemappedUncrawled != 0 || empty.CrawledNotInSitemap != 0 ||
		empty.SitemappedUnadmitted != 0 || len(empty.SampleUncrawled) != 0 || len(empty.SampleNotInSitemap) != 0 {
		t.Errorf("no-sitemap site coverage not zero-value: %+v", empty)
	}
}

// TestSitemapCoverageSampleCap proves each sample bucket is capped at 10.
func TestSitemapCoverageSampleCap(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://example.com", Name: "Example", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	crawled := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 25; i++ {
		seedSitemapURL(t, db, siteID, "https://example.com/uncrawled/"+strconv.Itoa(i), true, nil)
		seedSitemapURL(t, db, siteID, "https://example.com/orphan/"+strconv.Itoa(i), false, &crawled)
	}
	if _, err := db.SaveFileSnapshot(ctx, model.FileSnapshot{
		SiteID: siteID, Kind: model.FileKindSitemap, FetchedAt: time.Now().UTC(),
		ContentSHA256: "h", ParsedEntries: `{"v":1,"coverage":{"sitemapped_unadmitted":0}}`, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("SaveFileSnapshot: %v", err)
	}
	cov, err := db.SitemapCoverage(ctx, siteID)
	if err != nil {
		t.Fatalf("SitemapCoverage: %v", err)
	}
	if cov.SitemappedUncrawled != 25 {
		t.Errorf("SitemappedUncrawled count = %d, want 25", cov.SitemappedUncrawled)
	}
	if len(cov.SampleUncrawled) != 10 {
		t.Errorf("SampleUncrawled len = %d, want 10 (capped)", len(cov.SampleUncrawled))
	}
	if len(cov.SampleNotInSitemap) != 10 {
		t.Errorf("SampleNotInSitemap len = %d, want 10 (capped)", len(cov.SampleNotInSitemap))
	}
}
