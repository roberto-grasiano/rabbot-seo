package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/segments"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestReconcileSyncsAndClassifiesSegments guards A7 slice 3: reconcileSites syncs
// each site's segment definitions into the DB, reclassifies the site's URLs, and
// rebuilds + atomically swaps the in-memory registry so a config edit converges
// definitions, memberships, AND the hot-path lookup with no daemon restart.
// Adding, re-patterning, and removing a segment all converge; a removed segment
// leaves zero url_segments rows (FK cascade).
func TestReconcileSyncsAndClassifiesSegments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	fetch := fetcher.New(fetcher.Options{UserAgent: "t", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true})
	logger := obs.NewLogger(nil, "error")
	reg := segments.NewRegistry()

	// First reconcile: one site, one segment "content" => ^/blog/. Seed two URLs.
	cfg := config.Defaults()
	cfg.Sites = []config.SiteConfig{{
		URL:      "https://ex.com",
		Name:     "Ex",
		Segments: []config.SegmentConfig{{Name: "content", Match: "^/blog/"}},
	}}
	now := time.Now().UTC()
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites: %v", rerr)
	}
	site, err := db.GetSiteByBaseURL(ctx, "https://ex.com")
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	// Seed a /blog/ URL (member) and a /product/ URL (non-member).
	if _, uerr := db.UpsertURL(ctx, model.URL{SiteID: site.ID, URL: "https://ex.com/blog/post", FirstSeen: now, NextCheckAt: now, Interval: 600}); uerr != nil {
		t.Fatal(uerr)
	}
	if _, uerr := db.UpsertURL(ctx, model.URL{SiteID: site.ID, URL: "https://ex.com/product/x", FirstSeen: now, NextCheckAt: now, Interval: 600}); uerr != nil {
		t.Fatal(uerr)
	}
	// Reconcile again so ReclassifySite picks up the two URLs just seeded.
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites #2: %v", rerr)
	}

	segs, err := db.ListSegments(ctx, &site.ID)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 1 || segs[0].Name != "content" || segs[0].MemberCount != 1 {
		t.Fatalf("after sync, want one segment content with 1 member, got %+v", segs)
	}
	// Registry was swapped and answers the hot-path lookup.
	if got := reg.SegmentsFor(site.ID, "https://ex.com/blog/post"); len(got) != 1 || got[0] != "content" {
		t.Errorf("registry SegmentsFor = %v, want [content]", got)
	}
	if got := reg.SegmentsFor(site.ID, "https://ex.com/product/x"); got != nil {
		t.Errorf("non-member should match nothing, got %v", got)
	}

	// Re-pattern "content" to ^/product/ and ADD "blog" => ^/blog/. Membership flips.
	cfg.Sites[0].Segments = []config.SegmentConfig{
		{Name: "content", Match: "^/product/"},
		{Name: "blog", Match: "^/blog/"},
	}
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites #3: %v", rerr)
	}
	segs, _ = db.ListSegments(ctx, &site.ID)
	got := map[string]int{}
	for _, s := range segs {
		got[s.Name] = s.MemberCount
	}
	if got["content"] != 1 || got["blog"] != 1 {
		t.Errorf("after re-pattern+add, want content=1 (product) blog=1 (blog), got %v", got)
	}

	// Remove all segments: definitions and memberships both drop to zero (cascade).
	cfg.Sites[0].Segments = nil
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites #4: %v", rerr)
	}
	segs, _ = db.ListSegments(ctx, &site.ID)
	if len(segs) != 0 {
		t.Errorf("after removing all segments, want 0 definitions, got %+v", segs)
	}
	if got := reg.SegmentsFor(site.ID, "https://ex.com/blog/post"); got != nil {
		t.Errorf("after removal, registry should match nothing, got %v", got)
	}
}

// TestReconcileRecordsHealthScoreOnResegmentation covers the A6/A7 coordination
// seam: a reconcile that (re-)classifies a site's URLs into segments must record a
// site-scoped health score (whole site + every segment) so a re-segmentation is a
// scored event — the new per-segment trend starts at reconcile, not only at the
// first recheck of a member URL.
func TestReconcileRecordsHealthScoreOnResegmentation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	fetch := fetcher.New(fetcher.Options{UserAgent: "t", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true})
	logger := obs.NewLogger(nil, "error")
	reg := segments.NewRegistry()
	now := time.Now().UTC()

	cfg := config.Defaults()
	cfg.Sites = []config.SiteConfig{{
		URL:      "https://ex.com",
		Name:     "Ex",
		Segments: []config.SegmentConfig{{Name: "content", Match: "^/blog/"}},
	}}
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites: %v", rerr)
	}
	site, err := db.GetSiteByBaseURL(ctx, "https://ex.com")
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}

	// Seed processed, importance-bearing URLs so the score is DEFINED (above the
	// coverage floor): a /blog/ member and a /product/ non-member, both crawled.
	lc := now.Add(-time.Hour)
	for _, p := range []string{"https://ex.com/blog/post", "https://ex.com/product/x"} {
		if _, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: site.ID, URL: p, FirstSeen: now, LastChecked: &lc,
			NextCheckAt: now, Interval: 600, Importance: 1.0,
			StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
		}); uerr != nil {
			t.Fatal(uerr)
		}
	}

	// Reconcile again: ReclassifySite now sees the two URLs and the coordination
	// seam records a site-scoped health score (site + the "content" segment).
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, reg); rerr != nil {
		t.Fatalf("reconcileSites #2: %v", rerr)
	}

	siteSeries, err := db.HealthScoreSeries(ctx, site.ID, nil, time.Time{})
	if err != nil {
		t.Fatalf("HealthScoreSeries(site): %v", err)
	}
	if len(siteSeries) == 0 {
		t.Fatal("re-segmentation must record a whole-site health score row")
	}
	segs, err := db.ListSegments(ctx, &site.ID)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("want one segment, got %+v", segs)
	}
	segSeries, err := db.HealthScoreSeries(ctx, site.ID, &segs[0].ID, time.Time{})
	if err != nil {
		t.Fatalf("HealthScoreSeries(seg): %v", err)
	}
	if len(segSeries) == 0 {
		t.Fatal("re-segmentation must record the per-segment health score row (scored event)")
	}
}

// TestReconcileSites exercises the config->DB sync (decision S1): a configured
// site is inserted+enabled, its base URL is seeded due, and a sitemap-discovered
// URL is added. When the site is dropped from config a second reconcile disables
// it (history retained), without deleting its rows.
func TestReconcileSites(t *testing.T) {
	t.Parallel()

	// httptest server: serves a one-entry urlset at /sitemap.xml, 200 elsewhere.
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>` + srv.URL + `/about</loc><priority>0.8</priority></url>
</urlset>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// AllowPrivate so the fetcher's SSRF guard permits the 127.0.0.1 httptest host.
	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "test-agent",
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	logger := obs.NewLogger(nil, "error")

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites: %v", err)
	}

	// Site row exists and is enabled.
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	if !site.Enabled {
		t.Errorf("site should be enabled after reconcile")
	}

	// Base URL seeded and due now (next_check_at <= now).
	base, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("base URL not seeded: %v", err)
	}
	if base.NextCheckAt.After(now) {
		t.Errorf("base URL not due: next_check_at=%v now=%v", base.NextCheckAt, now)
	}
	if base.Depth != 0 {
		t.Errorf("base URL depth = %d, want 0", base.Depth)
	}

	// Sitemap-discovered URL added via Discoverer (wired in Task 9); disc=nil here
	// so sitemap seeding is skipped in this test. Only base URL is expected.

	// Drop the site from config and reconcile again: it must be disabled, not deleted.
	cfg.Sites = nil
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, time.Now().UTC(), logger, nil); err != nil {
		t.Fatalf("reconcileSites (drop): %v", err)
	}

	site2, err := db.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite after drop: %v", err)
	}
	if site2.Enabled {
		t.Errorf("dropped site should be disabled (history retained), still enabled")
	}
	// History/url rows retained.
	if _, err := db.GetURL(ctx, site.ID, srv.URL); err != nil {
		t.Errorf("base URL row should be retained after drop, got: %v", err)
	}
}

// TestReconcileAppliesThrottle pins that reconcile resolves per-site interval +
// concurrency through the verification-aware throttle gate: a StateThrottled site
// reconciles to the widened (>=30m) interval and concurrency 1, and once its
// proof flips to StateVerified a re-reconcile restores the config/default values.
func TestReconcileAppliesThrottle(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "test-agent",
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	logger := obs.NewLogger(nil, "error")

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "Test Site"}}

	now := time.Now().UTC()
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites (initial): %v", err)
	}

	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}

	// A freshly inserted site reads back StateThrottled (migration DEFAULT), so the
	// throttle floor must already apply: min_interval widened, concurrency clamped.
	throttledMin := int64((30 * time.Minute).Seconds())
	if site.MinInterval < throttledMin {
		t.Errorf("throttled MinInterval = %ds, want >= %ds", site.MinInterval, throttledMin)
	}
	if site.MaxConcurrency != 1 {
		t.Errorf("throttled MaxConcurrency = %d, want 1", site.MaxConcurrency)
	}

	// Flip the proof to verified, reconcile again: full/config values restored.
	if err := db.SaveVerification(ctx, site.ID, verify.ProofRecord{
		SiteID: site.ID, Method: verify.MethodWellKnown, Token: "rab_x",
		State: verify.StateVerified, VerifiedAt: now, LastReverifiedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerification: %v", err)
	}
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, time.Now().UTC(), logger, nil); err != nil {
		t.Fatalf("reconcileSites (verified): %v", err)
	}
	site2, err := db.GetSite(ctx, site.ID)
	if err != nil {
		t.Fatalf("GetSite (verified): %v", err)
	}
	wantMin := int64(cfg.MinIntervalDuration().Seconds()) // 10m default
	if site2.MinInterval != wantMin {
		t.Errorf("verified MinInterval = %ds, want %ds", site2.MinInterval, wantMin)
	}
	if site2.MaxConcurrency != 2 {
		t.Errorf("verified MaxConcurrency = %d, want 2", site2.MaxConcurrency)
	}
}

// floorIsInstalled reports whether the frontier is spacing host with a multi-second
// floor: it consumes the host's first (immediate) rate token, then asserts a second
// Acquire blocks past a 200ms deadline. With a sub-second base the second Acquire
// returns almost instantly; with the >=60s throttle floor it blocks. This observes
// the floor end-to-end (frontier internals stay unexported, PR31 #5) the same way
// scheduler/crawl_test.go asserts the robots Crawl-delay floor.
func floorIsInstalled(t *testing.T, front *frontier.Frontier, host string) bool {
	t.Helper()
	rel, err := front.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("first Acquire(%s): %v", host, err)
	}
	rel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rel2, err := front.Acquire(ctx, host)
	rel2()
	return err != nil // blocked past the deadline => a multi-second floor is in effect
}

// rateIsAtMost reports whether the frontier spaces host at no more than bound: it
// consumes the host's first (immediate) token, then asserts a second Acquire
// SUCCEEDS within bound. With the ~2s verified base a 3s bound succeeds; with the
// >=60s throttle floor it would block past bound. Companion to floorIsInstalled,
// which proves the opposite (a multi-second floor IS in effect).
func rateIsAtMost(t *testing.T, front *frontier.Frontier, host string, bound time.Duration) bool {
	t.Helper()
	rel, err := front.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("first Acquire(%s): %v", host, err)
	}
	rel()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	rel2, err := front.Acquire(ctx, host)
	rel2()
	return err == nil // completed within bound => rate is at most `bound`, not the 60s floor
}

// TestThrottledHostRateApplied pins that applyThrottleFloor translates a resolved
// crawl budget into the live frontier's throttle floor: a throttled site installs
// the >=60s floor; a verified site clears it (returning to the ~2s base); and a
// promotion (re-resolving the SAME host as verified) LOWERS the throttle floor
// back to the base without a restart (PR31 #3). The frontier-internal floor
// composition (robots vs throttle, raise/lower) is asserted white-box in
// internal/frontier; here the wiring is observed end-to-end via Acquire timing.
func TestThrottledHostRateApplied(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults() // unverified floor per_host_rate 60s
	cfg.Crawler.ContactEmail = "ops@example.com"
	// Lower the verified base to 300ms (still above the 250ms MinPerHostRate sanity
	// floor, and far below the 60s unverified throttle floor). This keeps the
	// base-vs-60s-floor distinction load-bearing while slashing the wall-clock: the
	// verified/promotion probes wait ~300ms instead of ~2s, with far more -race slack
	// than the old 3s deadlines against a 2s base.
	cfg.Defaults.PerHostRate = "300ms"

	// Seed the frontier from the same config base run.go uses (300ms here).
	baseRate, baseConc := frontierBaseFromConfig(&cfg)
	front := frontier.New(frontier.Options{PerHostRate: baseRate, PerHostConcurrency: baseConc})

	// Throttled host: the >=60s floor is installed (a second Acquire blocks).
	throttledEff := cfg.ResolveCrawl(config.SiteConfig{}, verify.StateThrottled)
	if !throttledEff.Throttled {
		t.Fatalf("expected throttled resolution")
	}
	applyThrottleFloor(front, "throttled.example", throttledEff)
	if !floorIsInstalled(t, front, "throttled.example") {
		t.Errorf("throttled host: expected the >=60s throttle floor to block a second Acquire")
	}

	// Verified host: the resolved base rate (300ms here) is installed via SetHostRate
	// — NOT cleared. It is rate-limited at the base, well under the >=60s throttled
	// floor. We assert the base tier by probing that the wait is NOT a 60s wait: a
	// 1s-deadline Acquire succeeds at the 300ms base but would block at the 60s floor.
	verifiedEff := cfg.ResolveCrawl(config.SiteConfig{}, verify.StateVerified)
	applyThrottleFloor(front, "verified.example", verifiedEff)
	if !rateIsAtMost(t, front, "verified.example", time.Second) {
		t.Errorf("verified host: expected the 300ms base rate (not the >=60s throttle floor)")
	}

	// Promotion: a throttled host re-resolved as verified must DROP from the >=60s
	// floor to the base WITHOUT a restart (PR31 #3) — the host rate is lowered, not
	// merely cleared to an unbounded base.
	applyThrottleFloor(front, "throttled.example", verifiedEff)
	if !rateIsAtMost(t, front, "throttled.example", time.Second) {
		t.Errorf("after promotion: expected the host rate lowered to the base, but a <=1s Acquire still blocked")
	}
}

// TestFrontierBaseFromConfigDefault pins that the daemon seeds the frontier's
// per-host base from the CONFIGURED defaults (resolved via ResolveCrawl for a
// verified empty site), not a hardcoded 2s/2 — an operator who sets
// defaults.per_host_rate must see it honored as the un-set-host base. With stock
// defaults this is the 2s/2 base (behavior-preserving); with a tuned default it
// follows config.
func TestFrontierBaseFromConfigDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults() // per_host_rate "2s" (Phase 1 D5), per_host_concurrency 2
	rate, conc := frontierBaseFromConfig(&cfg)
	if rate != 2*time.Second {
		t.Errorf("default base rate = %v, want 2s", rate)
	}
	if conc != 2 {
		t.Errorf("default base concurrency = %d, want 2", conc)
	}

	// A tuned per_host_rate is honored (and a tuned concurrency).
	cfg.Defaults.PerHostRate = "5s"
	cfg.Defaults.PerHostConcurrency = 4
	rate, conc = frontierBaseFromConfig(&cfg)
	if rate != 5*time.Second {
		t.Errorf("tuned base rate = %v, want 5s", rate)
	}
	if conc != 4 {
		t.Errorf("tuned base concurrency = %d, want 4", conc)
	}
}

// seedVerified writes a StateVerified proof record onto the site row so the
// verification-aware throttle (verificationState -> GetVerification) reads the
// authoritative verified tier — the only way to honor the per-site speed dial
// (faking verified in config does nothing). Mirrors store/verification_test.go.
func seedVerified(t *testing.T, ctx context.Context, db *store.DB, siteID int64) {
	t.Helper()
	now := time.Now().UTC()
	rec := verify.ProofRecord{
		SiteID:           siteID,
		Method:           verify.MethodWellKnown,
		Token:            "rab_TESTTOKENVALUE",
		State:            verify.StateVerified,
		VerifiedAt:       now,
		LastReverifiedAt: now,
	}
	if err := db.SaveVerification(ctx, siteID, rec); err != nil {
		t.Fatalf("SaveVerification(site %d): %v", siteID, err)
	}
}

// TestVerifiedSpeedDialAppliedToFrontier pins spec D2 end-to-end: a verified site's
// `speed` dial actually changes the live frontier's per-host spacing via
// installThrottleFloors -> ResolveCrawl -> SetHostRate. speed:200 halves the 2s base
// to ~1s (well under the 60s throttle floor and above the 250ms sanity floor);
// speed:50 doubles it to ~4s. Both are verified, so the dial is honored (an
// unverified site's >=60s floor would mask it). Observed via Acquire timing because
// frontier internals are unexported (PR31 #5).
func TestVerifiedSpeedDialAppliedToFrontier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "speed.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults() // base per_host_rate "2s"
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{
		{URL: "https://fast.example", Speed: 200}, // ~1s
		{URL: "https://slow.example", Speed: 50},  // ~4s
	}

	logger := obs.NewLogger(nil, "error")
	fetch := fetcher.New(fetcher.Options{AllowPrivate: true})
	// Reconcile config -> DB so both sites exist as rows (inserted StateThrottled).
	if rerr := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, time.Now().UTC(), logger, nil); rerr != nil {
		t.Fatalf("reconcileSites: %v", rerr)
	}
	// Promote both sites to StateVerified in the proof record so the speed dial is
	// honored (verificationState reads the authoritative DB proof).
	for _, baseURL := range []string{"https://fast.example", "https://slow.example"} {
		s, gerr := db.GetSiteByBaseURL(ctx, baseURL)
		if gerr != nil {
			t.Fatalf("GetSiteByBaseURL(%s): %v", baseURL, gerr)
		}
		seedVerified(t, ctx, db, s.ID) // helper: writes a StateVerified proof record
	}

	// Build the frontier from the REAL config defaults (2s base, NOT a tiny
	// placeholder): frontierBaseFromConfig is exactly how run.go seeds it. This makes
	// the speed dial LOAD-BEARING — an un-scaled host would sit at the 2s base, so a
	// fast assertion below 2s only passes if the dial actually lowered the base.
	baseRate, baseConc := frontierBaseFromConfig(&cfg)
	if baseRate != 2*time.Second {
		t.Fatalf("frontierBaseFromConfig rate = %v, want the real 2s default base", baseRate)
	}
	front := frontier.New(frontier.Options{PerHostRate: baseRate, PerHostConcurrency: baseConc})
	installThrottleFloors(ctx, db, &cfg, front, logger)

	// fast (speed:200 => ~1s): it IS rate-limited (a 200ms-deadline second Acquire
	// blocks), but a 1.5s-deadline second Acquire SUCCEEDS — proving the dial HALVED
	// the 2s base to ~1s. A 2s-unscaled host (the no-op bug) would block past 1.5s,
	// so this bound is what catches a regression of the headline fix.
	if !floorIsInstalled(t, front, "fast.example") {
		t.Errorf("fast(speed:200): expected ~1s spacing to block a 200ms Acquire")
	}
	if !rateIsAtMost(t, front, "fast.example", 1500*time.Millisecond) {
		t.Errorf("fast(speed:200): expected the dial to halve the 2s base to ~1s (<=1.5s); a 2s base would block past this")
	}

	// slow (speed:50 => ~4s): a 2s-deadline second Acquire BLOCKS (4s > 2s), proving
	// the slower dial spaces wider than the fast site.
	if rateIsAtMost(t, front, "slow.example", 2*time.Second) {
		t.Errorf("slow(speed:50): expected ~4s spacing to block a 2s Acquire")
	}
}

// TestDiscoveryResolverThrottlesMaxPages pins that the discovery MaxPages budget
// honors the verification-aware throttle: a StateThrottled site is clamped to the
// floor (<=50) while a StateVerified site gets the full configured budget (2000).
func TestDiscoveryResolverThrottlesMaxPages(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: "https://throttled.example"}, {URL: "https://verified.example"}}

	state := map[int64]verify.State{
		1: verify.StateThrottled,
		2: verify.StateVerified,
	}
	getState := func(id int64) verify.State { return state[id] }

	resolve := discoveryResolver(&mu, &cfg, getState)

	throttled := resolve(model.Site{ID: 1, BaseURL: "https://throttled.example"})
	if throttled.MaxPages > 50 {
		t.Errorf("throttled MaxPages = %d, want <= 50", throttled.MaxPages)
	}
	verified := resolve(model.Site{ID: 2, BaseURL: "https://verified.example"})
	if verified.MaxPages != 2000 {
		t.Errorf("verified MaxPages = %d, want 2000", verified.MaxPages)
	}
	// FollowLinks/Sitemap/MaxDepth come from ResolveDiscovery, unaffected by state.
	if !verified.FollowLinks || !verified.Sitemap {
		t.Errorf("verified discovery caps lost non-throttled fields: %+v", verified)
	}
}

// TestReconcileSitesValidatesSitemapLocs asserts the base URL is always seeded
// regardless of sitemap content. Sitemap loc SSRF validation (F32) is now the
// Discoverer's responsibility (tested in internal/discovery); reconcile with
// disc=nil skips sitemap seeding entirely, so no loc is ever attempted.
func TestReconcileSitesValidatesSitemapLocs(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>` + srv.URL + `/ok</loc><priority>0.8</priority></url>
  <url><loc>file:///etc/passwd</loc><priority>0.9</priority></url>
</urlset>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>home</title></head><body>hi</body></html>"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "test-agent",
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	logger := obs.NewLogger(nil, "error")

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL}}

	now := time.Now().UTC()
	if err := reconcileSites(ctx, db, &cfg, "0.0.1", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites: %v", err)
	}

	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}

	// Base URL always seeded regardless of sitemap content.
	if _, err := db.GetURL(ctx, site.ID, srv.URL); err != nil {
		t.Errorf("base URL should be seeded, got: %v", err)
	}
	// With disc=nil, no sitemap locs are ever attempted (sitemap seeding is skipped).
	if _, err := db.GetURL(ctx, site.ID, "file:///etc/passwd"); err == nil {
		t.Error("hostile non-http sitemap loc was seeded; should not be present with disc=nil")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetURL(hostile loc): unexpected error: %v", err)
	}
}
