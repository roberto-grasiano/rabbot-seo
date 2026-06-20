package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/supervisor"
)

// TestHealthScoreDaemonE2E is A6 acceptance criterion 5: end-to-end, a recheck
// that OPENS an issue leaves a health_scores row with score < 100 for the site.
//
// It wires the production post-fetch processor (supervisor.BuildAlertingStack →
// scheduler.Crawler.Processor) so the A6 compute-trigger seam fires on every
// successful rules pass exactly as the daemon runs it. The homepage crawls clean
// first (importance 1.0, no open issues → a defined score of 100, which records
// no row because storage moves only on change), then flips to noindex on a second
// crawl: the noindex rule opens a critical issue, RecordHealthScore recomputes a
// moved score and persists a row strictly below 100.
//
// The logical clock is advanced between the two crawls so the alert dedup window
// does not suppress the second pass's rules work (the recheck must run end-to-end).
func TestHealthScoreDaemonE2E(t *testing.T) {
	t.Parallel()

	var noindex atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		robots := ""
		if noindex.Load() {
			robots = `<meta name="robots" content="noindex">`
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title>` +
			`<meta name="description" content="welcome to the homepage of this site">` +
			`<link rel="canonical" href="` + r.Host + `/">` + robots +
			`</head><body><h1>Hello</h1><p>home page content words here today now</p></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "health_daemon.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := obs.NewLogger(nil, "error")

	// A logical clock advanced explicitly between phases (never wall-clock) so the
	// second crawl is outside the alert dedup window — the rules pass (and the
	// health-score recompute) must actually run on the recheck.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	cfg := config.Defaults()
	cfg.Crawler.ContactEmail = "ops@example.com"
	cfg.Sites = []config.SiteConfig{{URL: srv.URL}}

	stack, err := supervisor.BuildAlertingStack(cfg, db, srv.Client(), clock, logger, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	ua := "Rabbot-SEO/test"
	robots := frontier.NewRobotsCache(srv.Client(), ua, time.Minute)
	front := frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4})
	fetch := fetcher.New(fetcher.Options{UserAgent: ua, Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true})
	crawler := &scheduler.Crawler{
		Store:     db,
		Fetcher:   fetch,
		Extractor: extract.NewExtractor(),
		Robots:    robots,
		Frontier:  front,
		Now:       clock,
		Processor: stack.Processor, // diff -> rules -> alerts -> health-score record
		Logger:    logger,
	}

	if err := reconcileSites(ctx, db, &cfg, "test", fetch, nil, now, logger, nil); err != nil {
		t.Fatalf("reconcileSites: %v", err)
	}
	site, err := db.GetSiteByBaseURL(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetSiteByBaseURL: %v", err)
	}
	home, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL(home): %v", err)
	}

	// ── crawl #1: clean homepage → defined score 100, no open issues ──────────
	if r := crawler.CrawlOne(ctx, home, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne #1 (clean): %v", r.Err)
	}
	score, err := db.ComputeHealthScore(ctx, site.ID, nil)
	if err != nil {
		t.Fatalf("ComputeHealthScore (clean): %v", err)
	}
	if !score.Defined || score.Score != 100 {
		t.Fatalf("after a clean crawl the live site score must be defined 100, got %+v", score)
	}

	// ── crawl #2: homepage goes noindex → opens a critical issue ──────────────
	noindex.Store(true)
	now = now.Add(2 * time.Hour) // advance past the dedup window
	home2, err := db.GetURL(ctx, site.ID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL(home, pre-flip): %v", err)
	}
	if r := crawler.CrawlOne(ctx, home2, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne #2 (noindex): %v", r.Err)
	}

	// The recheck opened a critical issue on the homepage.
	open, err := db.ListIssues(ctx, store.IssueFilter{URLID: &home2.ID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues(home): %v", err)
	}
	if len(open) == 0 {
		t.Fatalf("the noindex flip must open at least one issue, got none")
	}

	// The compute-trigger seam persisted a whole-site health_scores row < 100.
	series, err := db.HealthScoreSeries(ctx, site.ID, nil, time.Time{})
	if err != nil {
		t.Fatalf("HealthScoreSeries(site): %v", err)
	}
	if len(series) == 0 {
		t.Fatalf("a recheck that opens an issue must leave a health_scores row for the site")
	}
	last := series[len(series)-1]
	if last.Score >= 100 {
		t.Fatalf("the persisted health score after opening an issue must be < 100, got %v (series=%+v)", last.Score, series)
	}
	if last.ComputedAt.Location() != time.UTC {
		t.Fatalf("persisted computed_at must be UTC, got %v", last.ComputedAt.Location())
	}

	// The live recomputed score agrees (errors-as-data sanity, not a fake number).
	live, err := db.ComputeHealthScore(ctx, site.ID, nil)
	if err != nil {
		t.Fatalf("ComputeHealthScore (post-flip): %v", err)
	}
	if !live.Defined || live.Score >= 100 || live.OpenCritical < 1 {
		t.Fatalf("post-flip live score must be defined, < 100, with >=1 open critical, got %+v", live)
	}
}
