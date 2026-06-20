package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/discovery"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/supervisor"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestDiscoveryAwareDaemonE2E proves the WIRED, discovery-aware daemon path end to
// end through the production seams — the one path none of the three existing E2E
// tests cover:
//
//   - TestDaemonE2E (this package) drives reconcileSites + a scheduler tick but
//     passes disc=nil, so discovery never runs through the daemon. It is a focused
//     M1-only crawl test (base URL only, no alerting).
//   - TestDiscoveryEndToEnd (internal/discovery) proves sitemap-index expansion,
//     the global page cap, and link-following — but with a HAND-BUILT disc
//     (newDisc), a deliberately TIGHT cap, and NO alerting. It exercises the
//     Discoverer in isolation, not the reconcile/crawl wiring run.go assembles.
//   - TestRealChangeFiresSlackAlert (internal/scheduler) proves
//     change->diff->rules->Slack, but only on the single pre-seeded SEED URL — no
//     discovery is involved at all.
//
// This test unifies them: it builds a REAL Discoverer the way run.go does (reusing
// the package-private discoveryResolver against a live config) and the real
// alerting stack via supervisor.BuildAlertingStack, then drives the production
// reconcileSites seam and live scheduler ticks against one httptest origin. It
// asserts that (a) discovery seeds MULTIPLE pages — sitemap-index expansion plus
// bounded same-host link-following — through reconcile + crawl, NOT just the seed,
// and (b) a later change to a DISCOVERED (non-seed, sitemap-sourced) page fires the
// full diff->rules->Slack alert chain. Generous default caps (MaxPages=2000,
// FollowLinks/Sitemap on) are used precisely so every page is admitted — the
// opposite of TestDiscoveryEndToEnd's cap-bounded design — because here we want to
// prove full discovery, not the bound.
//
// Network is confined to httptest loopback, so every fetcher/robots cache here sets
// AllowPrivate / uses srv.Client(); the production SSRF guard rejects 127.0.0.1
// otherwise. The logical clock (crawler/scheduler/discoverer) is pinned so the
// next_check_at math is deterministic; the frontier's per-host rate limiter still
// runs on the real clock but is set permissive (1ms spacing, 4-wide) so its
// real-time wait is negligible and the run stays deterministic without
// test-controlled sleeps. A small bounded tick loop drains the second discovery
// wave (link-following enqueues after the homepage crawl).
func TestDiscoveryAwareDaemonE2E(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// a1Noindex flips the discovered /a1 page to noindex once set, so a re-crawl of a
	// DISCOVERED page produces a real indexability regression (mirrors
	// realchange_e2e_test's toggle).
	var a1Noindex atomic.Bool

	// ── httptest origin ──────────────────────────────────────────────────────
	// srv is referenced inside handlers via closure, so declare first, assign after.
	var srv *httptest.Server
	mux := http.NewServeMux()

	// robots.txt: allow all + declare the sitemap index via a Sitemap: directive,
	// proving the directive is read (not the bare /sitemap.xml fallback).
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap_index.xml\n"))
	})

	// sitemap index → TWO child sitemaps (proves index expansion through reconcile).
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sitemap><loc>` + srv.URL + `/sm1.xml</loc></sitemap>` +
			`<sitemap><loc>` + srv.URL + `/sm2.xml</loc></sitemap>` +
			`</sitemapindex>`))
	})
	// child sitemap 1: a1, a2.
	mux.HandleFunc("/sm1.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/a1</loc><priority>0.9</priority></url>` +
			`<url><loc>` + srv.URL + `/a2</loc><priority>0.8</priority></url>` +
			`</urlset>`))
	})
	// child sitemap 2: b1, b2.
	mux.HandleFunc("/sm2.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/b1</loc><priority>0.7</priority></url>` +
			`<url><loc>` + srv.URL + `/b2</loc><priority>0.6</priority></url>` +
			`</urlset>`))
	})

	// homepage: a real indexable page linking to a same-host /orphan page (NOT in any
	// sitemap → proves bounded link-following) plus ONE external href that must never
	// be enqueued (same-host scope).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title>` +
			`<meta name="description" content="welcome to the homepage of this site">` +
			`</head><body><h1>Hello</h1><p>home page content words here today now</p>` +
			`<a href="` + srv.URL + `/orphan">orphan</a>` +
			`<a href="https://external.example.com/page">ext</a>` +
			`</body></html>`))
	})

	// /a1 is the controllable discovered page: clean+indexable until a1Noindex flips,
	// then it ships a noindex robots meta.
	mux.HandleFunc("/a1", func(w http.ResponseWriter, _ *http.Request) {
		robots := ""
		if a1Noindex.Load() {
			robots = `<meta name="robots" content="noindex">`
		}
		_, _ = w.Write([]byte(`<html><head><title>A1</title>` +
			`<meta name="description" content="the a1 page from sitemap one">` +
			`<link rel="canonical" href="` + srv.URL + `/a1">` + robots +
			`</head><body><h1>A1</h1><p>a1 page content words here today now</p></body></html>`))
	})

	// Every remaining sitemap/linked page is a real indexable SEO page so it crawls
	// clean (title + meta description present).
	for _, path := range []string{"/a2", "/b1", "/b2", "/orphan"} {
		p := path // capture for the closure
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html><head><title>` + p + `</title>` +
				`<meta name="description" content="page ` + p + ` real words for indexing">` +
				`</head><body><h1>` + p + `</h1><p>` + p + ` content words here today now</p></body></html>`))
		})
	}

	srv = httptest.NewServer(mux)
	defer srv.Close()

	// ── store + config + logger ──────────────────────────────────────────────
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "discovery_daemon.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := obs.NewLogger(nil, "error")

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL}}
	// Defaults give FollowLinks=true, Sitemap=true, MaxDepth=3, MaxPages=2000 — a
	// generous budget so EVERY discovered page is admitted (the opposite of the
	// tight cap in discovery/e2e_test.go).

	// ── real Discoverer, built exactly as run.go does ────────────────────────
	// Reuse the package-private discoveryResolver so Resolve is byte-identical to
	// production (live cfg under a mutex, per-site override merged over defaults),
	// then swap in loopback-capable fetchers + robots cache (the SSRF guard rejects
	// 127.0.0.1 otherwise), exactly as discovery/e2e_test.go does.
	var cfgMu sync.Mutex
	ua := "Rabbot-SEO/test"
	loopbackPages := fetcher.New(fetcher.Options{UserAgent: ua, Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true})
	loopbackSitemaps := fetcher.New(fetcher.Options{UserAgent: ua, Timeout: 5 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: true})
	disc := &discovery.Discoverer{
		Store:    db,
		Pages:    loopbackPages,
		Sitemaps: loopbackSitemaps,
		Robots:   frontier.NewRobotsCache(srv.Client(), ua, time.Minute),
		// Treat the test site as verified so discovery runs at the full configured
		// budget (this E2E predates the verification-aware throttle).
		Resolve: discoveryResolver(&cfgMu, &cfg, func(int64) verify.State { return verify.StateVerified }),
		Now:     clock,
		Logger:  logger,
	}

	// ── alerting stack, built exactly as run.go does ─────────────────────────
	// Mock Slack webhook counts POSTs.
	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	cfg.Notifiers = []config.NotifierConfig{{Name: "slack-critical", Type: "slack-webhook", URL: slackSrv.URL}}
	cfg.Routes = []config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}}
	stack, err := supervisor.BuildAlertingStack(cfg, db, slackSrv.Client(), clock, logger, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	// ── crawl pipeline, built exactly as run.go does (loopback-capable) ───────
	robots := frontier.NewRobotsCache(srv.Client(), ua, time.Minute)
	front := frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4})
	fetch := fetcher.New(fetcher.Options{UserAgent: ua, Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true})
	crawler := &scheduler.Crawler{
		Store:      db,
		Fetcher:    fetch,
		Extractor:  extract.NewExtractor(),
		Robots:     robots,
		Frontier:   front,
		Now:        clock,
		Processor:  stack.Processor, // diff -> rules -> alerts after each fetch
		Discoverer: disc,            // bounded same-host link-following after each extract
		Logger:     logger,
	}
	sched := &scheduler.Scheduler{
		DueStore:    db,
		CrawlFunc:   crawler.CrawlOne,
		Batch:       50,
		MinInterval: int64(cfg.MinIntervalDuration().Seconds()),
		MaxInterval: int64(cfg.MaxIntervalDuration().Seconds()),
		MaxParallel: 4,
		SelectorFor: func(model.URL) string { return "" },
		Now:         func() time.Time { return now },
		Log:         logger,
	}

	// ── (a) reconcile through the PRODUCTION seam with a real disc ────────────
	if err := reconcileSites(ctx, db, &cfg, "test", fetch, disc, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites: %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}

	// Discovery seeded MULTIPLE pages (base + 4 sitemap pages = 5), not just the seed.
	total, err := db.CountSiteURLs(ctx, site.ID)
	if err != nil {
		t.Fatalf("CountSiteURLs: %v", err)
	}
	if total <= 1 {
		t.Fatalf("reconcile with a real disc should seed multiple pages, got CountSiteURLs=%d (want > 1)", total)
	}
	// Both sitemap children fanned out through the index (a1 from sm1, b1 from sm2).
	if _, err := db.GetURL(ctx, site.ID, srv.URL+"/a1"); err != nil {
		t.Errorf("a1 (sitemap sm1) not seeded by reconcile+disc: %v", err)
	}
	if _, err := db.GetURL(ctx, site.ID, srv.URL+"/b1"); err != nil {
		t.Errorf("b1 (sitemap sm2) not seeded by reconcile+disc: %v", err)
	}

	// ── (b) drain the due inventory via real scheduler ticks ──────────────────
	// One tick crawls the base + sitemap pages. The homepage crawl then enqueues
	// /orphan (link-following), so run a small bounded loop driven by the due count
	// at the pinned `now` — never time-based — until no URL is due.
	for i := 0; i < 10; i++ {
		due, derr := db.CountDueURLs(ctx, now)
		if derr != nil {
			t.Fatalf("CountDueURLs: %v", derr)
		}
		if due == 0 {
			break
		}
		if terr := sched.Tick(ctx); terr != nil {
			t.Fatalf("Tick %d: %v", i, terr)
		}
	}

	// A DISCOVERED sitemap page (/a1) was driven through the real pipeline: snapshot
	// persisted with the expected title + 200.
	a1, err := db.GetURL(ctx, site.ID, srv.URL+"/a1")
	if err != nil {
		t.Fatalf("GetURL a1: %v", err)
	}
	a1Snap, err := db.LatestSnapshot(ctx, a1.ID)
	if err != nil {
		t.Fatalf("LatestSnapshot(a1): %v", err)
	}
	if a1Snap.Title != "A1" {
		t.Errorf("a1 snapshot Title = %q, want %q", a1Snap.Title, "A1")
	}
	if a1Snap.HTTPStatus != 200 {
		t.Errorf("a1 snapshot HTTPStatus = %d, want 200", a1Snap.HTTPStatus)
	}
	// The first crawl of /a1 must be clean+indexable so the noindex flip below has a
	// clean baseline to diff against.
	if !a1Snap.Indexable {
		t.Fatalf("a1's first crawl must be indexable (clean baseline for the flip), snap=%+v", a1Snap)
	}

	// /orphan (link-discovered from the homepage, not in any sitemap) was admitted
	// to the store by the LIVE crawler's bounded link-following.
	if _, err := db.GetURL(ctx, site.ID, srv.URL+"/orphan"); err != nil {
		t.Errorf("orphan (link-discovered, not in sitemap) should be admitted: %v", err)
	}
	// The external link must NEVER reach the store. In the live pipeline the
	// extractor drops cross-host <a href>s (extract.go appends only same-host links
	// to its returned slice), so an external URL never even reaches discovery's
	// same-host scope guard — this asserts the end-to-end property; discovery.admit's
	// own same-host check is unit-tested directly in internal/discovery.
	if _, eerr := db.GetURL(ctx, site.ID, "https://external.example.com/page"); !errors.Is(eerr, store.ErrNotFound) {
		t.Errorf("external link must not be in store; GetURL=%v, want ErrNotFound", eerr)
	}

	// No alert should have fired yet: every page crawled clean.
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("clean discovery crawl should not alert; slackHits=%d", got)
	}

	// ── (c) change a DISCOVERED page → full diff->rules->Slack chain ──────────
	// Flip /a1 (a sitemap-discovered, non-seed page) to noindex and re-crawl it
	// directly (deterministic, independent of reschedule timing).
	a1Noindex.Store(true)
	a1Row, err := db.GetURL(ctx, site.ID, srv.URL+"/a1")
	if err != nil {
		t.Fatalf("GetURL a1 (pre-flip): %v", err)
	}
	if r := crawler.CrawlOne(ctx, a1Row, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne a1 (noindex): %v", r.Err)
	}

	if got := atomic.LoadInt32(&slackHits); got == 0 {
		t.Fatalf("noindex regression on a DISCOVERED page must fire a Slack alert; slackHits=0")
	}

	open, err := db.ListIssues(ctx, store.IssueFilter{URLID: &a1Row.ID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues(a1): %v", err)
	}
	foundCritical := false
	for _, iss := range open {
		if iss.RuleID == "indexability_flip" && iss.Severity == model.SeverityCritical {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Errorf("expected an open critical indexability_flip issue on the discovered page a1, got %+v", open)
	}
}
