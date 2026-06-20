package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// stubFetcher returns a fixed Result for every Fetch, so a CrawlOne test can
// drive each FetchClass deterministically without a live origin.
type stubFetcher struct {
	res fetcher.Result
}

func (s stubFetcher) Fetch(ctx context.Context, req fetcher.Request) (fetcher.Result, error) {
	return s.res, nil
}
func (s stubFetcher) AllowsPrivate() bool { return true }

// Criterion 3a: CrawlOne records exactly one rabbot_fetches_total{class} per
// page fetch, for the class the fetcher reported (page fetches only in v1).
func TestCrawlOne_ObservesFetchPerClass(t *testing.T) {
	for _, fc := range []model.FetchClass{
		model.FetchOK, model.FetchSoftBlock, model.FetchHardBlock, model.FetchUnreachable,
	} {
		t.Run(string(fc), func(t *testing.T) {
			m := obs.NewMetrics("vtest")
			c := newStubbedCrawler(t, fetcher.Result{
				FetchClass:   fc,
				HTTPStatus:   200,
				ResponseTime: 12 * time.Millisecond,
				// No body: even FetchOK with an empty body skips extraction, so this
				// isolates the fetch counter from the snapshot/diff path.
			})
			c.Metrics = m

			u := model.URL{ID: 1, SiteID: 1, URL: "https://example.com/page", Interval: 600}
			c.CrawlOne(context.Background(), u, 600, 86400, "")

			want := fmt.Sprintf(`
				# HELP rabbot_fetches_total Total page fetches by access classification.
				# TYPE rabbot_fetches_total counter
				rabbot_fetches_total{class="%s"} 1
			`, fc)
			if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_fetches_total"); err != nil {
				t.Errorf("rabbot_fetches_total[%s] mismatch:\n%s", fc, err)
			}
			// The duration histogram observed exactly one sample.
			if got := testutil.CollectAndCount(m.Registry(), "rabbot_fetch_duration_seconds"); got != 1 {
				t.Errorf("rabbot_fetch_duration_seconds collected = %d, want 1", got)
			}
		})
	}
}

// A nil-metrics crawler must crawl normally and never panic.
func TestCrawlOne_NilMetricsSafe(t *testing.T) {
	c := newStubbedCrawler(t, fetcher.Result{FetchClass: model.FetchOK, HTTPStatus: 200})
	// c.Metrics is nil by default.
	u := model.URL{ID: 1, SiteID: 1, URL: "https://example.com/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
}

// newStubbedCrawler builds a Crawler whose fetcher returns res for every URL,
// with a robots cache that fails-open (no /robots.txt server) so the fetch path
// is always reached.
func newStubbedCrawler(t *testing.T, res fetcher.Result) *Crawler {
	t.Helper()
	fc := &fakeCrawlStore{
		saved:             map[int64]model.Snapshot{},
		latest:            map[int64]model.Snapshot{},
		updated:           map[int64]model.FetchClass{},
		scheduledInterval: map[int64]int64{},
		scheduledNextAt:   map[int64]time.Time{},
		scheduledETag:     map[int64]string{},
	}
	return &Crawler{
		Store:     fc,
		Fetcher:   stubFetcher{res: res},
		Extractor: extract.NewExtractor(),
		// Empty robots cache: no fetched robots.txt means fail-open Allowed==true.
		Robots:   frontier.NewRobotsCache(nil, "Rabbot-SEO/test", time.Minute),
		Frontier: frontier.New(frontier.Options{PerHostRate: time.Microsecond, PerHostConcurrency: 4}),
		Now:      func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
	}
}

// Criterion 3b: ProcessFetch adds rabbot_changes_total{class} per ChangeClass
// over the computed []model.Change — 1 cosmetic + 2 substantive => {1,2}.
func TestProcessFetch_AddsChangesPerClass(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m := obs.NewMetrics("vtest")
	deps := &fakeProcDeps{}
	p := NewProcessor(deps, 4, func() time.Time { return now }, WithMetrics(m))

	site := model.Site{ID: 1, BaseURL: "https://ex.com", Name: "Ex"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}

	// Build old/new snapshots whose diff yields exactly 1 cosmetic + 2 substantive
	// changes. title + meta_description are always-substantive; a near-identical
	// content body (below the simhash threshold) classifies cosmetic.
	// Content hash differs but both simhashes are non-zero and within the
	// threshold-4 Hamming distance (0x000F vs 0x001F = 1 differing bit), so the
	// content change classifies COSMETIC. title + meta_description are always
	// substantive. => 1 cosmetic + 2 substantive.
	old := model.Snapshot{
		ID: 1, URLID: 5, Title: "Old Title", MetaDescription: "old meta",
		ContentSHA256: "h1", ContentSimhash: 0x000F, Indexable: true, HTTPStatus: 200,
	}
	newSnap := model.Snapshot{
		ID: 2, URLID: 5, Title: "New Title", MetaDescription: "new meta",
		ContentSHA256: "h2", ContentSimhash: 0x001F, Indexable: true, HTTPStatus: 200,
	}

	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}

	// Assert the class split matches what diff.Compare actually recorded, so the
	// metric counts mirror the recorded change set exactly (no double counting).
	var cosmetic, substantive int
	for _, c := range deps.recorded {
		switch c.ChangeClass {
		case model.ChangeCosmetic:
			cosmetic++
		case model.ChangeSubstantive:
			substantive++
		}
	}
	if cosmetic != 1 || substantive != 2 {
		t.Fatalf("test fixture drift: recorded %d cosmetic + %d substantive, want 1 + 2 (changes=%+v)",
			cosmetic, substantive, deps.recorded)
	}

	want := `
		# HELP rabbot_changes_total Total detected changes by significance class.
		# TYPE rabbot_changes_total counter
		rabbot_changes_total{class="cosmetic"} 1
		rabbot_changes_total{class="substantive"} 2
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(want), "rabbot_changes_total"); err != nil {
		t.Errorf("rabbot_changes_total mismatch:\n%s", err)
	}
}

// Slice 5: rabbot_crawls_in_flight is a GaugeFunc over Scheduler.QueueDepth().
// Wiring SetInFlightFunc(sched.QueueDepth) makes the gauge reflect the live
// in-flight count on every scrape — and reads only the atomic counter (no DB).
func TestInFlightGauge_OverQueueDepth(t *testing.T) {
	ds := &blockingDueStore{due: []model.URL{
		{ID: 1, URL: "https://e.com/a", Interval: 600},
		{ID: 2, URL: "https://e.com/b", Interval: 600},
	}}

	entered := make(chan struct{})
	release := make(chan struct{})

	s := &Scheduler{
		DueStore:    ds,
		Batch:       10,
		MaxParallel: 8,
		MinInterval: 600,
		MaxInterval: 86400,
		CrawlFunc: func(ctx context.Context, u model.URL, minI, maxI int64, sel string) CrawlResult {
			entered <- struct{}{}
			<-release
			return CrawlResult{URLID: u.ID, FetchClass: model.FetchOK}
		},
		Now: time.Now,
	}

	m := obs.NewMetrics("vtest")
	// This is the production wiring run.go performs: the gauge reads QueueDepth on
	// every scrape (the atomic load — scrape-safe, no DB).
	m.SetInFlightFunc(s.QueueDepth)

	tickDone := make(chan error, 1)
	go func() { tickDone <- s.Tick(context.Background()) }()

	<-entered
	<-entered

	// Scrape mid-flight: the gauge reflects 2 in flight.
	const wantTwo = `
		# HELP rabbot_crawls_in_flight Crawls currently in flight (concurrent page fetches).
		# TYPE rabbot_crawls_in_flight gauge
		rabbot_crawls_in_flight 2
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(wantTwo), "rabbot_crawls_in_flight"); err != nil {
		t.Errorf("rabbot_crawls_in_flight (mid-flight) mismatch:\n%s", err)
	}

	close(release)
	if err := <-tickDone; err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// After the tick drains, the gauge returns to 0.
	const wantZero = `
		# HELP rabbot_crawls_in_flight Crawls currently in flight (concurrent page fetches).
		# TYPE rabbot_crawls_in_flight gauge
		rabbot_crawls_in_flight 0
	`
	if err := testutil.CollectAndCompare(m.Registry(), strings.NewReader(wantZero), "rabbot_crawls_in_flight"); err != nil {
		t.Errorf("rabbot_crawls_in_flight (drained) mismatch:\n%s", err)
	}
}

// A nil-metrics processor must process normally and never panic.
func TestProcessFetch_NilMetricsSafe(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deps := &fakeProcDeps{}
	p := NewProcessor(deps, 4, func() time.Time { return now }) // no metrics

	site := model.Site{ID: 1, BaseURL: "https://ex.com"}
	u := model.URL{ID: 5, SiteID: 1, URL: "https://ex.com/p", Importance: 1.0}
	old := model.Snapshot{ID: 1, URLID: 5, Title: "Old", Indexable: true, HTTPStatus: 200}
	newSnap := model.Snapshot{ID: 2, URLID: 5, Title: "New", Indexable: true, HTTPStatus: 200}
	if _, err := p.ProcessFetch(context.Background(), site, u, newSnap, old, model.FetchOK, "", false); err != nil {
		t.Fatalf("ProcessFetch: %v", err)
	}
}
