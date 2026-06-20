package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestDiscoveryEndToEnd drives the full discovery pipeline:
//   - robots.txt declares a sitemap index
//   - sitemap index → TWO child sitemaps with distinct pages
//     (sm1: a1,a2 / sm2: b1,b2,b3), each <loc> priority-tagged
//   - homepage / links to /orphan (same-host, not in sitemap) + an external URL
//
// MaxPages=5 with the base URL pre-seeded (count starts at 1) makes the cap a real
// constraint: the 5 sitemap pages cannot all fit, so the lowest-priority page (b3)
// is dropped, and the cap is then exhausted so the link stage admits nothing.
//
// Asserts:
//  1. Pages from BOTH children seed (a1 from sm1, b1 from sm2) — recursion fans out.
//  2. The over-budget page (b3) is dropped (ErrNotFound) — the cap genuinely bounds.
//  3. The external link is never in the store (different host).
//  4. The global cap spans stages: with sitemaps filling MaxPages, the homepage's
//     /orphan link is NOT admitted (the cap is one budget, not per-stage).
//  5. The total never exceeds MaxPages.
func TestDiscoveryEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Build the httptest server. srv is referenced inside handlers via closure so
	// we declare it first and assign after NewServer.
	var srv *httptest.Server
	mux := http.NewServeMux()

	// robots.txt: declares a sitemap index.
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap_index.xml\n"))
	})

	// sitemap index → TWO child sitemaps.
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sitemap><loc>` + srv.URL + `/sm1.xml</loc></sitemap>` +
			`<sitemap><loc>` + srv.URL + `/sm2.xml</loc></sitemap>` +
			`</sitemapindex>`))
	})

	// child sitemap 1: a1 (pri .9), a2 (pri .8).
	mux.HandleFunc("/sm1.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/a1</loc><priority>0.9</priority></url>` +
			`<url><loc>` + srv.URL + `/a2</loc><priority>0.8</priority></url>` +
			`</urlset>`))
	})

	// child sitemap 2: b1 (.7), b2 (.6), b3 (.5 — lowest, the over-budget drop).
	mux.HandleFunc("/sm2.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/b1</loc><priority>0.7</priority></url>` +
			`<url><loc>` + srv.URL + `/b2</loc><priority>0.6</priority></url>` +
			`<url><loc>` + srv.URL + `/b3</loc><priority>0.5</priority></url>` +
			`</urlset>`))
	})

	// homepage: links to /orphan (same-host, NOT in sitemap) and an external URL.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title></head><body>` +
			`<a href="` + srv.URL + `/orphan">orphan</a>` +
			`<a href="https://external.example.com/page">ext</a>` +
			`</body></html>`))
	})

	// Page bodies for every same-host path the crawler might reach.
	for _, path := range []string{"/a1", "/a2", "/b1", "/b2", "/b3", "/orphan"} {
		p := path // capture for closure
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html><head><title>` + p + `</title></head><body></body></html>`))
		})
	}

	srv = httptest.NewServer(mux)
	defer srv.Close()

	// Open store.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e_disc.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: srv.URL, Name: "E2E", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	site := model.Site{ID: siteID, BaseURL: srv.URL, MinInterval: 600}

	// MaxPages=5 with the base URL pre-seeded (count=1) leaves room for only 4 of the
	// 5 sitemap pages, so the lowest-priority page (b3) is dropped and the cap is then
	// exhausted — the link stage can admit nothing more.
	caps := Caps{FollowLinks: true, Sitemap: true, MaxDepth: 3, MaxPages: 5}
	disc, _ := newDisc(t, st, caps)

	// ── Seed the base URL so it's in the inventory (mimics what reconcile does). ──
	if _, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: srv.URL + "/", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	}); err != nil {
		t.Fatalf("UpsertURL base: %v", err)
	}

	// ── Step 1: SeedSitemaps ──────────────────────────────────────────────────
	// robots.txt declares /sitemap_index.xml → /sm1.xml (a1,a2) + /sm2.xml (b1,b2,b3).
	// Override the Robots field to use the test server's HTTP client so the
	// RobotsCache can reach the httptest loopback URL.
	disc.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)
	// Also override Sitemaps fetcher to AllowPrivate (loopback) + use srv.Client.
	disc.Sitemaps = fetcher.New(fetcher.Options{
		UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: true,
	})
	disc.Pages = disc.Sitemaps

	added, err := disc.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	// 5 sitemap pages, base already counts 1, cap=5 → only 4 admitted (b3 dropped).
	if added != 4 {
		t.Errorf("SeedSitemaps: want 4 pages admitted (a1,a2,b1,b2; b3 over budget), got %d", added)
	}
	// Pages from BOTH children must seed.
	if _, err := st.GetURL(ctx, siteID, srv.URL+"/a1"); err != nil {
		t.Errorf("a1 (child sm1) not seeded after SeedSitemaps: %v", err)
	}
	if _, err := st.GetURL(ctx, siteID, srv.URL+"/b1"); err != nil {
		t.Errorf("b1 (child sm2) not seeded after SeedSitemaps: %v", err)
	}
	// The lowest-priority page is over budget → dropped.
	if _, err := st.GetURL(ctx, siteID, srv.URL+"/b3"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("b3 should be dropped by the cap; GetURL=%v, want ErrNotFound", err)
	}

	// ── Step 2: CrawlOne the homepage, wired with the Discoverer ─────────────
	// The homepage links to /orphan (same-host) and an external URL. The sitemap
	// stage has already filled MaxPages, so the link stage admits neither: this
	// proves the cap is ONE global budget spanning sitemap + link discovery.
	crawler := &scheduler.Crawler{
		Store:      st,
		Fetcher:    fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor:  extract.NewExtractor(),
		Robots:     frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute),
		Frontier:   frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:        clock,
		Discoverer: disc,
	}

	u, err := st.GetURL(ctx, siteID, srv.URL+"/")
	if err != nil {
		t.Fatalf("GetURL homepage: %v", err)
	}
	if r := crawler.CrawlOne(ctx, u, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne homepage: %v", r.Err)
	}

	// ── Step 3: Assertions ───────────────────────────────────────────────────

	// /orphan must NOT be admitted: the global cap was already exhausted by the
	// sitemap stage, so the link stage adds nothing.
	if _, err := st.GetURL(ctx, siteID, srv.URL+"/orphan"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("orphan should be blocked by the exhausted global cap; GetURL=%v, want ErrNotFound", err)
	}

	// The external URL must NOT be enqueued (different host).
	_, extErr := st.GetURL(ctx, siteID, "https://external.example.com/page")
	if !errors.Is(extErr, store.ErrNotFound) {
		t.Errorf("external link should NOT be in store; GetURL returned: %v (want ErrNotFound)", extErr)
	}

	// MaxPages cap: total URLs for this site must be ≤ MaxPages (5) and, here,
	// exactly at the cap (base + a1 + a2 + b1 + b2).
	total, err := st.CountSiteURLs(ctx, siteID)
	if err != nil {
		t.Fatalf("CountSiteURLs: %v", err)
	}
	if total > caps.MaxPages {
		t.Errorf("page cap exceeded: total=%d > MaxPages=%d", total, caps.MaxPages)
	}
	if total != 5 {
		t.Errorf("want 5 URLs in store (base+a1+a2+b1+b2, cap-bound), got %d", total)
	}
}
