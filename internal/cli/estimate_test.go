package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/coverage"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
)

// TestRenderCoverageLineKnown asserts the human line carries the page count, the
// request rate, a full-pass duration, an on-disk footprint, the recheck cadence, and
// the speed-up nudge, for a known (>0) page count.
func TestRenderCoverageLineKnown(t *testing.T) {
	est := coverage.Estimate(10000, 2*time.Second)
	got := renderCoverageLine(10000, est, 10*time.Minute, 0, false)
	for _, want := range []string{
		"10000 pages",
		"req/s",
		"full pass",
		"on disk",
		"Rechecks every",
		"verify",
		"speed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderCoverageLine() missing %q:\n%s", want, got)
		}
	}
}

// TestRenderCoverageLineCapped pins Spec D8: when the resolved cap is below the
// estimated page count, the line states the partial coverage ("monitoring N of
// ~M") and flags it as capped; an uncapped (0 = unlimited) or cap >= pages line
// keeps the full "~M pages" phrasing.
func TestRenderCoverageLineCapped(t *testing.T) {
	est := coverage.Estimate(10000, 2*time.Second)

	// cap 2000 < 10000 pages, GLOBAL default (perSiteCap=false) → capped phrasing
	// + the `config set` remedy (which works for a global-default cap).
	capped := renderCoverageLine(10000, est, 0, 2000, false)
	if !strings.Contains(capped, "monitoring 2000 of ~10000") {
		t.Errorf("capped line must say 'monitoring 2000 of ~10000':\n%s", capped)
	}
	if !strings.Contains(capped, "capped") {
		t.Errorf("capped line must flag it as capped:\n%s", capped)
	}
	if !strings.Contains(capped, "config set defaults.discovery.max_pages_per_site") {
		t.Errorf("global-default cap must point at `config set`:\n%s", capped)
	}

	// Same cap but PER-SITE (perSiteCap=true): `config set defaults.…` would NOT
	// change it, so the remedy must point at the per-site key in config.yaml.
	perSite := renderCoverageLine(10000, est, 0, 2000, true)
	if !strings.Contains(perSite, "this site's discovery.max_pages_per_site in config.yaml") {
		t.Errorf("per-site cap must point at the per-site key in config.yaml:\n%s", perSite)
	}
	if strings.Contains(perSite, "config set") {
		t.Errorf("per-site cap must NOT suggest `config set` (it has no effect):\n%s", perSite)
	}

	// cap 0 (unlimited) → no capped phrasing, full coverage.
	unlimited := renderCoverageLine(10000, est, 0, 0, false)
	if strings.Contains(unlimited, "monitoring") || strings.Contains(unlimited, "capped") {
		t.Errorf("unlimited (cap 0) line must not be capped:\n%s", unlimited)
	}
	if !strings.Contains(unlimited, "~10000 pages") {
		t.Errorf("unlimited line must state the full page count:\n%s", unlimited)
	}

	// cap >= pages → not capped (cap doesn't bite).
	roomy := renderCoverageLine(500, coverage.Estimate(500, 2*time.Second), 0, 2000, false)
	if strings.Contains(roomy, "capped") {
		t.Errorf("cap above the page count must not be flagged capped:\n%s", roomy)
	}

	// page count unknown → unchanged guidance regardless of cap.
	unknown := renderCoverageLine(0, coverage.Result{}, 0, 2000, false)
	if !strings.Contains(unknown, "page count unknown") {
		t.Errorf("unknown count must keep the guidance line:\n%s", unknown)
	}
}

// TestRenderCoverageLineUnknown asserts the unknown-count path prints the spec's exact
// guidance instead of a fabricated estimate.
func TestRenderCoverageLineUnknown(t *testing.T) {
	got := renderCoverageLine(0, coverage.Result{}, 10*time.Minute, 0, false)
	if !strings.Contains(got, "page count unknown") {
		t.Errorf("renderCoverageLine(0, …) missing 'page count unknown':\n%s", got)
	}
	if !strings.Contains(got, "--pages") {
		t.Errorf("renderCoverageLine(0, …) missing '--pages' guidance:\n%s", got)
	}
}

// TestRenderCoverageLineProductionDefault drives the FULL production path —
// resolveCrawlBudget → coverage.Estimate → renderCoverageLine — for the DEFAULT config
// (per-host concurrency 2) and pins the exact rendered figures. The per-host admission
// ceiling is 1/per_host_rate = 0.5 req/s at the 2s default REGARDLESS of concurrency
// (the frontier's single rate.Limiter at burst 1), so the rendered rate must be
// "0.50 req/s" — never the concurrency-inflated "1.00". 10k pages at 0.5 req/s is a
// ~5h 33m full pass and ~117.2 MB on disk, matching the README example exactly.
func TestRenderCoverageLineProductionDefault(t *testing.T) {
	cfg := config.Defaults()
	rate, _, recheck := resolveCrawlBudget(&cfg, "https://example.test")
	got := renderCoverageLine(10000, coverage.Estimate(10000, rate), recheck, 0, false)

	for _, want := range []string{
		"~10000 pages",
		"~0.50 req/s",
		"full pass ≈ 5h 33m",
		"~117.2 MB on disk",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("production default coverage line missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1.00 req/s") {
		t.Errorf("production default coverage line overstates rate (concurrency leaked into req/s):\n%s", got)
	}
}

// TestHumanDuration is a direct table test for the compact duration renderer: above an
// hour shows "Xh Ym"; sub-hour shows "Ym Zs"; sub-minute shows "Zs"; non-positive is
// "0s".
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{20000 * time.Second, "5h 33m"},                      // >1h
		{2 * time.Hour, "2h 0m"},                             // exact hours
		{10*time.Minute + 30*time.Second, "10m 30s"},         // sub-hour min+sec
		{59 * time.Second, "59s"},                            // sub-minute
		{750 * time.Millisecond, "<1s"},                      // sub-second (tiny site at max speed)
		{250 * time.Millisecond, "<1s"},                      // a 1-page pass at the 250ms floor
		{0, "0s"},                                            // d == 0
		{-5 * time.Second, "0s"},                             // d < 0
		{time.Hour + 5*time.Minute + 9*time.Second, "1h 5m"}, // drops seconds above an hour
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCountSitemapPagesFromRobots fetches the robots.txt-declared sitemap, parses it,
// and returns the URL count — the auto-count the doctor command uses.
func TestCountSitemapPagesFromRobots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nSitemap: " + sitemapURLBase(r) + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset>` +
			`<url><loc>http://x/a</loc></url>` +
			`<url><loc>http://x/b</loc></url>` +
			`<url><loc>http://x/c</loc></url>` +
			`</urlset>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := fetcher.New(fetcher.Options{UserAgent: "test", AllowPrivate: true})
	n, ok := countSitemapPages(context.Background(), f, "test", srv.URL, true)
	if !ok {
		t.Fatalf("countSitemapPages() ok=false, want a count")
	}
	if n != 3 {
		t.Errorf("countSitemapPages() = %d, want 3", n)
	}
}

// TestCountSitemapPagesNoSitemap proves a site with no declared sitemap yields ok=false
// so the caller falls back to the "page count unknown" message.
func TestCountSitemapPagesNoSitemap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	f := fetcher.New(fetcher.Options{UserAgent: "test", AllowPrivate: true})
	if n, ok := countSitemapPages(context.Background(), f, "test", srv.URL, true); ok {
		t.Errorf("countSitemapPages() ok=true (n=%d), want false for no sitemap", n)
	}
}

// TestCountSitemapPagesBoundedDeadline proves the auto-count self-caps: a sitemap host
// that hangs every fetch cannot make the count wait out multiple 30s fetcher timeouts.
// The bounded child context (sitemapCountBudget) trips first, the count gives up, and
// the helper returns ok=false ("page count unknown") well under the serial-timeout
// worst case.
func TestCountSitemapPagesBoundedDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		// Declare several sitemaps so a naive serial count would wait out several
		// hangs (~one fetcher timeout each).
		_, _ = w.Write([]byte("User-agent: *\n" +
			"Sitemap: " + base + "/s1.xml\n" +
			"Sitemap: " + base + "/s2.xml\n" +
			"Sitemap: " + base + "/s3.xml\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Every sitemap fetch blocks until the request context is cancelled.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := fetcher.New(fetcher.Options{UserAgent: "test", AllowPrivate: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, ok := countSitemapPages(context.Background(), f, "test", srv.URL, true); ok {
			t.Errorf("countSitemapPages() ok=true (n=%d), want false when every fetch hangs", n)
		}
	}()
	select {
	case <-done:
	case <-time.After(sitemapCountBudget + 10*time.Second):
		t.Fatalf("countSitemapPages did not self-cap within %v + slack", sitemapCountBudget)
	}
}

// sitemapURLBase returns the scheme://host of the test server from the request, so the
// robots.txt Sitemap: line points back at the same httptest server.
func sitemapURLBase(r *http.Request) string {
	return "http://" + r.Host
}

// TestResolveCrawlBudgetDefault proves the estimator's budget for an unconfigured site
// under the default config is the 2s base at the verified tier (Phase 1 D5) — so a
// 10k-page estimate is exactly 0.5 req/s, matching the spec's worked example and
// guaranteeing no silent slowdown when no per_host_rate is set.
func TestResolveCrawlBudgetDefault(t *testing.T) {
	cfg := config.Defaults()
	rate, conc, _ := resolveCrawlBudget(&cfg, "https://example.test")
	if rate != 2*time.Second {
		t.Errorf("default per-host rate = %v, want 2s (D5 behavior-preserving default)", rate)
	}
	if conc != 2 {
		t.Errorf("default per-host concurrency = %d, want 2", conc)
	}
	est := coverage.Estimate(10000, rate)
	if est.ReqPerSec != 0.5 {
		t.Errorf("ReqPerSec at default budget = %v, want 0.5", est.ReqPerSec)
	}
}

// TestEmitCoverageForAddedSites proves the init summary prints a coverage line for a
// freshly added site whose sitemap is countable. It is best-effort: a site with no
// sitemap simply prints the unknown-count guidance, never an error.
func TestEmitCoverageForAddedSites(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nSitemap: http://" + r.Host + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset>` +
			`<url><loc>http://x/a</loc></url><url><loc>http://x/b</loc></url></urlset>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "t"}}

	var buf bytes.Buffer
	emitCoverageForAddedSites(context.Background(), &buf, &cfg, []string{srv.URL}, "9.9.9", true)
	out := buf.String()
	if !strings.Contains(out, "Coverage:") {
		t.Errorf("emitCoverageForAddedSites missing Coverage line:\n%s", out)
	}
	if !strings.Contains(out, "2 pages") {
		t.Errorf("emitCoverageForAddedSites missing the sitemap page count:\n%s", out)
	}
}

// TestEmitCoverageForAddedSitesThreadsVersion proves the build version reaches the
// self-identifying User-Agent of the sitemap auto-count fetch (finding #9): a "" version
// would emit "Rabbot-SEO/ (…)" with an empty version segment. The served robots/sitemap
// handlers capture the User-Agent header and assert it carries the real version.
func TestEmitCoverageForAddedSitesThreadsVersion(t *testing.T) {
	var gotUA string
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("User-agent: *\nSitemap: http://" + r.Host + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset>` +
			`<url><loc>http://x/a</loc></url></urlset>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL, Name: "t"}}

	var buf bytes.Buffer
	emitCoverageForAddedSites(context.Background(), &buf, &cfg, []string{srv.URL}, "7.7.7", true)
	if !strings.Contains(gotUA, "Rabbot-SEO/7.7.7 (+mailto:ops@example.com") {
		t.Errorf("sitemap-count UA = %q, want it to carry version 7.7.7", gotUA)
	}
}

// TestProductionCountPagesThreadsVersion proves the build version reaches the
// wizard's CountPages fetch User-Agent (finding #9), via the same served-UA capture.
func TestProductionCountPagesThreadsVersion(t *testing.T) {
	var gotUA string
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("User-agent: *\nSitemap: http://" + r.Host + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset>` +
			`<url><loc>http://x/a</loc></url></urlset>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	count := productionCountPagesFor(&cfg, "7.7.7", true)
	if _, ok := count(context.Background(), srv.URL); !ok {
		t.Fatalf("count failed against %s", srv.URL)
	}
	if !strings.Contains(gotUA, "Rabbot-SEO/7.7.7 (+mailto:ops@example.com") {
		t.Errorf("CountPages UA = %q, want it to carry version 7.7.7", gotUA)
	}
}

// TestEmitCoverageForAddedSitesCapped pins Spec D8 end-to-end through the headless
// emit path: a site whose sitemap declares MORE URLs than its configured per-site
// cap must render the partial-coverage "monitoring N of ~M … (capped …)" phrasing,
// proving the cap (written via SetSiteMaxPagesYAML) flows through ResolveDiscovery
// into renderCoverageLine. The cap (3) is set below the served sitemap count (5).
func TestEmitCoverageForAddedSitesCapped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nSitemap: http://" + r.Host + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset>` +
			`<url><loc>http://x/a</loc></url>` +
			`<url><loc>http://x/b</loc></url>` +
			`<url><loc>http://x/c</loc></url>` +
			`<url><loc>http://x/d</loc></url>` +
			`<url><loc>http://x/e</loc></url></urlset>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Seed a one-site config on disk, then set the per-site cap to 3 EXACTLY as the
	// Spec B planner does (SetSiteMaxPagesYAML), and reload so ResolveDiscovery sees it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	base := "crawler:\n  contact_email: ops@me.example\nsites:\n  - url: " + srv.URL + "\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SetSiteMaxPagesYAML(cfgPath, srv.URL, 3); err != nil {
		t.Fatalf("SetSiteMaxPagesYAML: %v", err)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	var buf bytes.Buffer
	emitCoverageForAddedSites(context.Background(), &buf, &cfg, []string{srv.URL}, "9.9.9", true)
	out := buf.String()
	// 3 < 5 → the line states partial coverage and flags it capped (Spec D8).
	if !strings.Contains(out, "monitoring 3 of") {
		t.Errorf("capped emit missing 'monitoring 3 of':\n%s", out)
	}
	if !strings.Contains(out, "~5 pages") {
		t.Errorf("capped emit missing the full sitemap count '~5 pages':\n%s", out)
	}
	if !strings.Contains(out, "capped") {
		t.Errorf("capped emit must flag it as capped:\n%s", out)
	}
}
