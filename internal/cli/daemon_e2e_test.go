package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestDaemonE2E exercises the full M1 crawl path end to end against an httptest
// server, with no real timers: reconcileSites seeds the inventory (base URL),
// then one manually-driven scheduler tick pops the due URLs and runs them through
// the live crawl pipeline (frontier -> robots -> fetch -> extract -> persist ->
// reschedule). It asserts the homepage snapshot was extracted and stored, and
// that the tick advanced next_check_at so the due-count drops.
//
// This test deliberately passes disc=nil to stay a focused M1-only crawl test
// (base URL seeded, no sitemap/link discovery). The discovery-aware daemon path —
// a real Discoverer driving multi-page sitemap + link discovery through
// reconcileSites, plus an alert on a discovered page — is covered separately by
// TestDiscoveryAwareDaemonE2E.
//
// Network is confined to httptest (loopback); the fetcher therefore sets
// AllowPrivate so its SSRF guard permits the 127.0.0.1 host (the production
// default rejects loopback). Scheduling time is pinned via Scheduler.Now to a
// fixed instant so the pop is deterministic.
func TestDaemonE2E(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>` + srv.URL + `/</loc><priority>1.0</priority></url>
  <url><loc>` + srv.URL + `/about</loc><priority>0.8</priority></url>
</urlset>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>About</title>` +
			`<meta name="description" content="About this site"></head>` +
			`<body><p>about page content words here today now</p></body></html>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title>` +
			`<meta name="description" content="Welcome home"></head>` +
			`<body><p>home page content words here today now</p></body></html>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rabbot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := obs.NewLogger(nil, "error")

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL}}

	// Pipeline components. AllowPrivate lets the fetcher's SSRF guard reach the
	// loopback httptest host; the robots cache uses the server's own client.
	robots := frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Minute)
	front := frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4})
	fetch := fetcher.New(fetcher.Options{
		UserAgent:    "Rabbot-SEO/test",
		Timeout:      5 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	ext := extract.NewExtractor()

	now := time.Now().UTC()

	// Reconcile: seed the base URL due now. disc=nil here keeps this test M1-only,
	// so no sitemap/link discovery runs and only the base URL is seeded (the wired
	// discovery path is covered by TestDiscoveryAwareDaemonE2E).
	if err := reconcileSites(ctx, db, &cfg, "test", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites: %v", err)
	}

	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}

	// Base URL seeded.
	if _, err := db.GetURL(ctx, site.ID, srv.URL); err != nil {
		t.Fatalf("base URL not seeded: %v", err)
	}

	dueBefore, err := db.CountDueURLs(ctx, now)
	if err != nil {
		t.Fatalf("CountDueURLs (before): %v", err)
	}
	if dueBefore < 1 {
		t.Fatalf("CountDueURLs (before) = %d, want >= 1 (base URL due)", dueBefore)
	}

	// Build the crawl pipeline + scheduler. Now is pinned so the pop is
	// deterministic; CrawlOne reschedules off real wall-clock time, so every
	// crawled URL's next_check_at lands strictly after `now`.
	crawler := &scheduler.Crawler{
		Store:     db,
		Fetcher:   fetch,
		Extractor: ext,
		Robots:    robots,
		Frontier:  front,
	}
	sched := &scheduler.Scheduler{
		DueStore:    db,
		CrawlFunc:   crawler.CrawlOne,
		Batch:       50,
		MinInterval: 60,
		MaxInterval: 3600,
		MaxParallel: 4,
		Now:         func() time.Time { return now },
	}

	// One manual tick drives the whole batch through the pipeline.
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Homepage was fetched, extracted, and persisted.
	home, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL home: %v", err)
	}
	snap, err := db.LatestSnapshot(ctx, home.ID)
	if err != nil {
		t.Fatalf("LatestSnapshot(home): %v", err)
	}
	if snap.Title != "Home" {
		t.Errorf("home snapshot Title = %q, want %q", snap.Title, "Home")
	}
	if snap.HTTPStatus != 200 {
		t.Errorf("home snapshot HTTPStatus = %d, want 200", snap.HTTPStatus)
	}

	// The tick advanced next_check_at: every popped URL was rescheduled into the
	// future (off real wall-clock), so the due-count at the pinned `now` drops.
	dueAfter, err := db.CountDueURLs(ctx, now)
	if err != nil {
		t.Fatalf("CountDueURLs (after): %v", err)
	}
	if dueAfter >= dueBefore {
		t.Errorf("CountDueURLs did not drop: before=%d after=%d (next_check_at should have advanced)", dueBefore, dueAfter)
	}
}
