package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// sitemapURLStore adapts the concrete *store.DB to scheduler.URLStore for the
// sitemap-watch e2e (the same adaptation supervisor.SitemapURLStore performs in
// production; redeclared locally so the discovery test never imports supervisor).
type sitemapURLStore struct{ db *store.DB }

func (s sitemapURLStore) ReconcileSitemapMembership(ctx context.Context, siteID int64, locs []string, additiveOnly bool) error {
	return s.db.ReconcileSitemapMembership(ctx, siteID, locs, additiveOnly)
}

func (s sitemapURLStore) SitemapLiveCounts(ctx context.Context, siteID int64) (scheduler.SitemapLiveCounts, error) {
	c, err := s.db.SitemapLiveCounts(ctx, siteID)
	if err != nil {
		return scheduler.SitemapLiveCounts{}, err
	}
	return scheduler.SitemapLiveCounts{
		SitemappedUncrawled: c.SitemappedUncrawled,
		CrawledNotInSitemap: c.CrawledNotInSitemap,
		InSitemapTotal:      c.InSitemapTotal,
	}, nil
}

// coverageHookFor maps store.SitemapCoverage onto the control wire DTO (the same
// mapping cli.coverageHook performs; inlined to avoid importing internal/cli).
func coverageHookFor(db *store.DB) func(context.Context, int64) (control.CoverageResponse, bool, error) {
	return func(ctx context.Context, siteID int64) (control.CoverageResponse, bool, error) {
		res, err := db.SitemapCoverage(ctx, siteID)
		if err != nil {
			return control.CoverageResponse{}, false, err
		}
		return control.CoverageResponse{
			HasSitemap:           res.HasSitemap,
			SeedStatus:           res.SeedStatus,
			SitemappedUncrawled:  res.SitemappedUncrawled,
			SitemappedUnadmitted: res.SitemappedUnadmitted,
			CrawledNotInSitemap:  res.CrawledNotInSitemap,
			SampleUncrawled:      res.SampleUncrawled,
			SampleNotInSitemap:   res.SampleNotInSitemap,
		}, true, nil
	}
}

// TestSitemapWatchEndToEnd drives the full sitemap-watch loop end to end against an
// httptest origin, proving acceptance criterion 14:
//
//   - robots.txt declares /sitemap.xml; the sitemap is served from a flippable
//     fixture (pass 1: /a,/b,/c — pass 2: /a,/b,/d, dropping /c and adding /d).
//   - Pass 1 RefreshSitemap is the alert-silent baseline (no prior snapshot).
//   - Between passes the clock advances past the dedup window so the second pass's
//     events are real incidents, not deduped away (the A3+A5 round's durable lesson:
//     per-incident dedup needs CLOCK ADVANCE between e2e phases).
//   - Pass 2 flips the fixture and re-runs: a sitemap_xml set-change warning flows
//     all the way through the real alerts.Pipeline into the incidents store, and
//     GET /v1/coverage (the real control endpoint over store.SitemapCoverage)
//     reflects the reconciled state.
//
// It exercises the production seams unmodified: discovery.Discoverer.CollectAndSeed
// feeds SideTimers.RefreshSitemap, which reconciles urls.in_sitemap, persists the
// FileKindSitemap snapshot, diffs, and ingests into the pipeline.
func TestSitemapWatchEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Advanceable clock: pass 2 runs an hour later so the dedup window (5m) is well
	// past and the set-change event is a fresh, dispatched incident.
	var nowNS atomic.Int64
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowNS.Store(base.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNS.Load()).UTC() }

	// Flippable sitemap fixture. flip==false → a,b,c; flip==true → a,b,d.
	var flip atomic.Bool
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap.xml\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		locs := []string{"/a", "/b", "/c"}
		if flip.Load() {
			locs = []string{"/a", "/b", "/d"}
		}
		body := `<?xml version="1.0" encoding="UTF-8"?>` +
			`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`
		for _, l := range locs {
			body += `<url><loc>` + srv.URL + l + `</loc><priority>0.7</priority></url>`
		}
		body += `</urlset>`
		_, _ = w.Write([]byte(body))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "sitemap_watch_e2e.db"))
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

	// Seed a crawled-but-NOT-in-sitemap URL (link-discovered, last_checked set) so
	// the crawled_not_in_sitemap coverage bucket is non-zero after reconcile.
	crawled := base
	if _, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: srv.URL + "/orphan", FirstSeen: base, LastChecked: &crawled,
		NextCheckAt: base, Interval: 600, Importance: 0.5,
	}); err != nil {
		t.Fatalf("UpsertURL orphan: %v", err)
	}

	// Discoverer wired to reach the httptest loopback (AllowPrivate + srv client).
	caps := Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000}
	disc, _ := newDisc(t, st, caps)
	disc.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)
	disc.Sitemaps = fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: true})
	disc.Pages = disc.Sitemaps

	// Real alerts stack: notifier → mock Slack, match-all route, dispatcher, pipeline.
	var slackHits int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()
	notifier := notify.NewSlackNotifier("slack-critical", slackSrv.URL, slackSrv.Client())
	registry := notify.NewRegistry(
		map[string]notify.Notifier{"slack-critical": notifier},
		[]config.RouteConfig{{Match: map[string]string{}, Notifier: "slack-critical"}},
	)
	pipeline := alerts.NewPipeline(st, notify.NewDispatcher(registry),
		alerts.WithCaps(alerts.Caps{DedupWindow: 5 * time.Minute, HourlyCap: 30, IncidentAutoClose: 24 * time.Hour}),
		alerts.WithClock(clock),
	)

	side := &scheduler.SideTimers{
		FileStore: st,
		Sitemaps:  disc,
		URLStore:  sitemapURLStore{db: st},
		Alerts:    pipeline,
		Now:       clock,
	}

	// ── Pass 1: baseline. a,b,c admitted; snapshot persisted; ZERO events. ──────
	if err := side.RefreshSitemap(ctx, site); err != nil {
		t.Fatalf("RefreshSitemap pass 1: %v", err)
	}
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("baseline pass must not alert; slackHits=%d", got)
	}
	opens, err := st.ListOpenIncidents(ctx)
	if err != nil {
		t.Fatalf("ListOpenIncidents after pass 1: %v", err)
	}
	if len(opens) != 0 {
		t.Fatalf("baseline pass must open no incidents, got %d (%+v)", len(opens), opens)
	}
	// The three declared locs are admitted and flagged in_sitemap=1.
	for _, p := range []string{"/a", "/b", "/c"} {
		u, gerr := st.GetURL(ctx, siteID, srv.URL+p)
		if gerr != nil {
			t.Fatalf("GetURL %s after pass 1: %v", p, gerr)
		}
		if !u.InSitemap {
			t.Errorf("%s in_sitemap = false after pass 1, want true", p)
		}
	}

	// ── Advance the clock past the dedup window, then flip the fixture. ─────────
	nowNS.Store(base.Add(time.Hour).UnixNano())
	flip.Store(true)

	// ── Pass 2: set change (drop /c, add /d) → one sitemap_xml warning. ─────────
	if err := side.RefreshSitemap(ctx, site); err != nil {
		t.Fatalf("RefreshSitemap pass 2: %v", err)
	}
	if got := atomic.LoadInt32(&slackHits); got == 0 {
		t.Fatalf("set-change pass must fire an alert through the pipeline; slackHits=0")
	}
	opens, err = st.ListOpenIncidents(ctx)
	if err != nil {
		t.Fatalf("ListOpenIncidents after pass 2: %v", err)
	}
	var sawSetChange bool
	for _, inc := range opens {
		if inc.GroupKey == alerts.GroupKey(srv.URL, "sitemap_xml") {
			sawSetChange = true
		}
	}
	if !sawSetChange {
		t.Fatalf("expected an open sitemap_xml incident after the set change, got %+v", opens)
	}

	// Reconcile flipped membership: /c dropped (1→0), /d added (admitted, in_sitemap=1).
	uc, err := st.GetURL(ctx, siteID, srv.URL+"/c")
	if err != nil {
		t.Fatalf("GetURL /c after pass 2: %v", err)
	}
	if uc.InSitemap {
		t.Errorf("/c in_sitemap = true after drop, want false (full read flips off)")
	}
	ud, err := st.GetURL(ctx, siteID, srv.URL+"/d")
	if err != nil {
		t.Fatalf("GetURL /d after pass 2: %v", err)
	}
	if !ud.InSitemap {
		t.Errorf("/d in_sitemap = false after add, want true")
	}

	// ── GET /v1/coverage reflects the reconciled state. ─────────────────────────
	ctrl := control.NewServer(control.ServerOptions{
		Token: "tok", Version: "0.1.0",
		Hooks: control.Hooks{Coverage: coverageHookFor(st)},
	})
	cs := httptest.NewServer(ctrl.Handler())
	defer cs.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		cs.URL+"/v1/coverage?site_id="+strconv.FormatInt(siteID, 10), nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/coverage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/coverage status = %d, want 200", resp.StatusCode)
	}
	var cov control.CoverageResponse
	if derr := json.NewDecoder(resp.Body).Decode(&cov); derr != nil {
		t.Fatalf("decode coverage: %v", derr)
	}
	if !cov.HasSitemap {
		t.Errorf("coverage.has_sitemap = false, want true (sitemap is watched)")
	}
	if cov.SeedStatus != 200 {
		t.Errorf("coverage.seed_status = %d, want 200", cov.SeedStatus)
	}
	// The seeded /orphan is crawled (last_checked set) and never in the sitemap, so
	// it is the one crawled-but-absent URL. The sitemapped pages were admitted but
	// never crawled (last_checked NULL), so all three are sitemapped-but-uncrawled.
	if cov.CrawledNotInSitemap != 1 {
		t.Errorf("coverage.crawled_not_in_sitemap = %d, want 1 (the orphan)", cov.CrawledNotInSitemap)
	}
	if cov.SitemappedUncrawled != 3 {
		t.Errorf("coverage.sitemapped_uncrawled = %d, want 3 (a,b,d admitted, never crawled)", cov.SitemappedUncrawled)
	}
}
