package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestRunDaemonEagerSitemapCoverage is the regression guard for issue #88: a
// newly-added site that correctly declares a sitemap (robots.txt `Sitemap:`
// directive and/or `/sitemap.xml`) must report has_sitemap=true SHORTLY AFTER
// daemon start — not 24h later.
//
// Root cause being guarded: has_sitemap is true iff a FileKindSitemap snapshot
// exists (store.SitemapCoverage), and the ONLY writer of that snapshot is
// SideTimers.RefreshSitemap, whose ONLY caller is the sitemap-refresh ticker in
// run.go — a time.NewTicker(SitemapRefresh) (default 24h) whose first tick fires a
// full interval later (Go ticker semantics: it does NOT fire at startup).
// Site-add/reconcile only calls SeedSitemaps (admits URLs, never persists the
// snapshot). So before the fix, coverage is dark for ~24h on every fresh site.
//
// The fix triggers ONE eager RefreshSitemap pass at daemon start (a refreshAll()
// helper called once before the periodic for/select loop), so the FileKindSitemap
// snapshot lands on the first crawl cycle. This test drives the REAL daemon
// (runDaemon) with a SHORT TickInterval against a loopback httptest origin that
// declares a sitemap, then polls the production read path (control GET
// /v1/coverage) until has_sitemap flips true within a few seconds.
//
// Before the run.go change this FAILS (has_sitemap stays false for the whole test
// window because only the 24h ticker would ever write the snapshot); after it,
// the eager refreshAll() writes the snapshot on the first cycle and it PASSES.
//
// Network is confined to httptest loopback, so the daemon runs with
// AllowPrivate=true (the production SSRF guard rejects 127.0.0.1 otherwise),
// exactly as the discovery / metrics / concurrency E2E tests do. State is read
// through the daemon's own control read path rather than opening a second
// connection to the DB the daemon holds open.
func TestRunDaemonEagerSitemapCoverage(t *testing.T) {
	// Loopback origin: robots.txt declares the sitemap via a Sitemap: directive
	// (proving the directive is read), a valid sitemap lists two real indexable
	// pages, and the pages crawl clean. srv is referenced in handlers via closure,
	// so declare first, assign after.
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/</loc><priority>1.0</priority></url>` +
			`<url><loc>` + srv.URL + `/about</loc><priority>0.8</priority></url>` +
			`</urlset>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>About</title>` +
			`<meta name="description" content="about this site real words here">` +
			`</head><body><h1>About</h1><p>about page content words here today now</p></body></html>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title>` +
			`<meta name="description" content="welcome to the homepage real words here">` +
			`</head><body><h1>Hello</h1><p>home page content words here today now</p></body></html>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// Second loopback origin: a site that declares NO sitemap at all — robots.txt
	// has no Sitemap: directive and the /sitemap.xml fallback 404s. The eager
	// RefreshSitemap pass collects an empty, INCOMPLETE set (a non-OK seed fetch),
	// and RefreshSitemap's first-ever-pass-incomplete guard
	// (sidetimers_sitemap.go) returns WITHOUT persisting a snapshot — so this
	// site's has_sitemap must stay false. This proves the eager pass preserves the
	// guard and never fabricates a bogus snapshot for a site with no sitemap.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n")) // no Sitemap: directive
	})
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sitemap.xml" {
			http.NotFound(w, r) // /sitemap.xml fallback 404s → no sitemap declared
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>NoMap</title>` +
			`<meta name="description" content="a site with no sitemap real words here">` +
			`</head><body><h1>NoMap</h1><p>no sitemap content words here today now</p></body></html>`))
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()

	// Grab a free port, then release it so the control server can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Seed a config with TWO sites: the first declares a sitemap (eager refresh
	// must make has_sitemap=true), the second declares none (eager refresh must
	// leave has_sitemap=false — the guard). Sitemap discovery is ON (the default).
	// follow_links is off to keep the crawl bounded and the assertion focused on
	// the sitemap snapshot landing.
	//
	// The site is UNVERIFIED (no proof record), so the daemon applies the
	// unverified throttle floor to its per-host spacing and gates the eager sitemap
	// BFS through that same frontier. The default unverified floor is per_host_rate
	// 60s — far longer than this test's window — so set a small POSITIVE override
	// (250ms, the MinPerHostRate sanity floor) for BOTH the base rate and the
	// unverified floor. A positive override is honored as the operator's tuned floor
	// (config/throttle.go), so the host still crawls POLITELY through the real
	// frontier (the production gating is preserved, not bypassed) — just fast enough
	// that the single sitemap.xml fetch completes within the assertion window.
	// Single-quoted YAML so any Windows backslash in dir stays literal.
	seed := "data_dir: '" + dir + "'\n" +
		"crawler:\n  contact_email: 'ops@example.com'\n" +
		"defaults:\n  min_interval: '1s'\n  max_interval: '1m'\n" +
		"  per_host_rate: '250ms'\n" +
		"  unverified_throttle:\n    per_host_rate: '250ms'\n" +
		"  discovery:\n    sitemap: true\n    follow_links: false\n" +
		"sites:\n  - url: '" + srv.URL + "'\n  - url: '" + srv2.URL + "'\n"
	if werr := os.WriteFile(cfgPath, []byte(seed), 0o600); werr != nil {
		t.Fatalf("seed config.yaml: %v", werr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         cfgPath,
			DataDir:            dir,
			ControlToken:       "tok",
			ControlPort:        port,
			Version:            "0.0.1",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
			AllowPrivate:       true, // admit the loopback httptest origin
		})
	}()
	// Always drain the daemon goroutine before the test exits.
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("runDaemon did not exit within 30s of cancel")
		}
	}()

	client := control.NewClient(port, "tok")

	// Wait for the control server to bind and serve (poll Health).
	deadline := time.Now().Add(30 * time.Second) // generous: windows-latest under -race needs >3s to start
	for {
		if herr := client.Health(context.Background()); herr == nil {
			break
		}
		select {
		case derr := <-done:
			done <- derr // re-buffer for the drain path
			t.Fatalf("daemon exited before becoming healthy: %v; logs:\n%s", derr, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("control server did not become healthy within 30s; logs:\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Resolve both seeded site ids via the daemon's own read path.
	var siteID, noMapSiteID int64
	sdeadline := time.Now().Add(15 * time.Second)
	for {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		sites, serr := client.Sites(sctx)
		scancel()
		if serr == nil {
			for _, s := range sites {
				switch s.URL {
				case srv.URL:
					siteID = s.ID
				case srv2.URL:
					noMapSiteID = s.ID
				}
			}
		}
		if siteID != 0 && noMapSiteID != 0 {
			break
		}
		if time.Now().After(sdeadline) {
			t.Fatalf("seeded sites %q / %q never both appeared via control /v1/sites (got ids %d / %d); logs:\n%s",
				srv.URL, srv2.URL, siteID, noMapSiteID, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Poll the production coverage read path until has_sitemap flips true. The
	// eager refreshAll() at daemon start drives one RefreshSitemap pass, which
	// collects the declared sitemap and persists the FileKindSitemap snapshot that
	// store.SitemapCoverage reads — so has_sitemap must become true within seconds
	// of startup, NOT 24h later. Without the eager refresh this loop exhausts its
	// deadline with has_sitemap=false (only the 24h ticker would ever write it).
	covDeadline := time.Now().Add(8 * time.Second)
	hasSitemap := false
	for time.Now().Before(covDeadline) {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		cov, found, cerr := client.Coverage(cctx, siteID)
		ccancel()
		if cerr == nil && found && cov.HasSitemap {
			hasSitemap = true
			break
		}
		select {
		case derr := <-done:
			done <- derr // re-buffer for the drain path
			t.Fatalf("daemon exited before coverage reported a sitemap: %v; logs:\n%s", derr, out.String())
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !hasSitemap {
		t.Fatalf("has_sitemap never became true within 8s of startup — the daemon did not run an eager RefreshSitemap pass, so coverage is dark for ~24h on a fresh site (issue #88).\nlogs:\n%s", out.String())
	}

	// Guard: the eager pass iterates EVERY enabled site in one sweep, so by the time
	// the declared-sitemap site reports has_sitemap=true, the no-sitemap site has
	// also been refreshed. Its /sitemap.xml 404s → the collection is incomplete on
	// the first-ever pass → RefreshSitemap returns WITHOUT writing a snapshot. So its
	// coverage must STILL report has_sitemap=false — the eager call never fabricates
	// a bogus snapshot for a site with no sitemap.
	nctx, ncancel := context.WithTimeout(context.Background(), 5*time.Second)
	noMapCov, found, cerr := client.Coverage(nctx, noMapSiteID)
	ncancel()
	if cerr != nil {
		t.Fatalf("Coverage(no-sitemap site): %v; logs:\n%s", cerr, out.String())
	}
	if !found {
		t.Fatalf("no-sitemap site %q not found via control /v1/coverage; logs:\n%s", srv2.URL, out.String())
	}
	if noMapCov.HasSitemap {
		t.Errorf("no-sitemap site reported has_sitemap=true — the eager RefreshSitemap pass persisted a bogus snapshot for a site with no sitemap (the incomplete/no-prior-snapshot guard was not preserved)")
	}
}
