package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestRealChangeFiresSlackAlert drives the REAL crawl pipeline (CrawlOne: robots
// -> fetch -> extract -> persist -> M2 process) against an httptest origin, with
// the alerting stack wired to a mock Slack server. It crawls a clean indexable
// page (no alert), mutates the page to ship `noindex`, re-crawls, and asserts a
// Slack POST fired — proving fetch->extract->diff->rules->alert end to end.
func TestRealChangeFiresSlackAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Origin server: serves robots + a page that flips to noindex when `noindex` is set.
	var noindex atomic.Bool
	var origin *httptest.Server
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
			`<meta name="description" content="welcome">` +
			`<link rel="canonical" href="` + origin.URL + `/">` + robots +
			`</head><body><h1>Hello</h1><p>some real words here today now</p></body></html>`))
	})
	origin = httptest.NewServer(mux)
	defer origin.Close()

	// Mock Slack webhook: count POSTs.
	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	siteID, err := st.AddSite(ctx, model.Site{
		BaseURL: origin.URL, Name: "Origin", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: origin.URL, FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	// Alerting stack (inline, mirroring scheduler/e2e_test.go): notifier -> mock Slack,
	// match-all route, dispatcher, pipeline, engine, processor.
	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)
	engine := rules.NewEngine(rules.DefaultRuleSet(), st, clock)
	proc := NewProcessor(&e2eDeps{store: st, engine: engine, pipeline: pipeline}, diff.DefaultSimhashThreshold, clock)

	// Real crawl pipeline. AllowPrivate lets the SSRF guard reach loopback.
	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(origin.Client(), "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:       clock,
		Processor: proc,
	}

	// Crawl 1: clean indexable page. No alert expected.
	u1, err := st.GetURL(ctx, siteID, origin.URL)
	if err != nil {
		t.Fatalf("GetURL u1: %v", err)
	}
	if r := crawler.CrawlOne(ctx, u1, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne 1: %v", r.Err)
	}
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("clean first crawl should not alert; slackHits=%d", got)
	}
	snap1, err := st.LatestSnapshot(ctx, urlID)
	if err != nil || !snap1.Indexable {
		t.Fatalf("crawl 1 snapshot should be indexable: snap=%+v err=%v", snap1, err)
	}

	// Mutate the origin: ship noindex.
	noindex.Store(true)

	// Crawl 2: noindex regression. Critical alert expected (immediate, not digested).
	u2, err := st.GetURL(ctx, siteID, origin.URL)
	if err != nil {
		t.Fatalf("GetURL u2: %v", err)
	}
	if r := crawler.CrawlOne(ctx, u2, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne 2: %v", r.Err)
	}
	if got := atomic.LoadInt32(&slackHits); got == 0 {
		t.Fatalf("noindex regression must fire a Slack alert; slackHits=0")
	}

	open, err := st.ListIssues(ctx, store.IssueFilter{URLID: &urlID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	foundCritical := false
	for _, iss := range open {
		if iss.RuleID == "indexability_flip" && iss.Severity == model.SeverityCritical {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Errorf("expected an open critical indexability_flip issue after the noindex flip, got %+v", open)
	}
}
