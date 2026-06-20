package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

// recordingDeps wraps the real e2eDeps (store + engine + pipeline) and records the
// alerts.Event values that flow through the ingest/resolve seam, so a test can assert
// on the exact change_type/severity that reached the alert pipeline — the spec's
// "ingested alert event" — while still driving the REAL engine, REAL alert pipeline,
// and a REAL (mock) Slack sink end to end.
type recordingDeps struct {
	inner *e2eDeps

	mu       sync.Mutex
	ingested []alerts.Event
	resolved []alerts.Event
}

func (d *recordingDeps) RecordChanges(ctx context.Context, c []model.Change) error {
	return d.inner.RecordChanges(ctx, c)
}

func (d *recordingDeps) ApplyRules(ctx context.Context, urlID int64, imp float64, newSnap, old model.Snapshot, ch []model.Change, truncated bool) ([]NewFinding, error) {
	return d.inner.ApplyRules(ctx, urlID, imp, newSnap, old, ch, truncated)
}

func (d *recordingDeps) HandleFetchClass(ctx context.Context, ac alerts.AccessContext, seo []alerts.Event) (bool, error) {
	return d.inner.HandleFetchClass(ctx, ac, seo)
}

func (d *recordingDeps) IngestEvent(ctx context.Context, e alerts.Event) error {
	d.mu.Lock()
	d.ingested = append(d.ingested, e)
	d.mu.Unlock()
	return d.inner.IngestEvent(ctx, e)
}

func (d *recordingDeps) RecordHealthScore(ctx context.Context, siteID, urlID int64) error {
	return d.inner.RecordHealthScore(ctx, siteID, urlID)
}

func (d *recordingDeps) ResolveEvent(ctx context.Context, e alerts.Event) error {
	d.mu.Lock()
	d.resolved = append(d.resolved, e)
	d.mu.Unlock()
	return d.inner.ResolveEvent(ctx, e)
}

func (d *recordingDeps) ingestedEvents() []alerts.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]alerts.Event, len(d.ingested))
	copy(out, d.ingested)
	return out
}

func (d *recordingDeps) resolvedEvents() []alerts.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]alerts.Event, len(d.resolved))
	copy(out, d.resolved)
	return out
}

// productPage renders a page carrying one Product JSON-LD block. When withOffers is
// true the Product ships `offers` (eligible under GRR202606: name + an any-of member);
// when false `offers` is dropped, so the Product loses rich-result eligibility while
// the @type set is UNCHANGED — the exact marquee regression that fires no schema_types
// diff and is invisible to type-set diffing.
func productPage(canonical string, withOffers bool) string {
	var ld string
	if withOffers {
		ld = `{"@context":"https://schema.org","@type":"Product","name":"Widget",` +
			`"offers":{"@type":"Offer","price":"19.99","priceCurrency":"USD"}}`
	} else {
		ld = `{"@context":"https://schema.org","@type":"Product","name":"Widget"}`
	}
	return `<html><head><title>Widget</title>` +
		`<meta name="description" content="a fine widget for sale">` +
		`<link rel="canonical" href="` + canonical + `">` +
		`<script type="application/ld+json">` + ld + `</script>` +
		`</head><body><h1>Widget</h1><p>some real product words here today now</p></body></html>`
}

// TestRichResultMarqueeMissingOffersE2E is the A4 MARQUEE DEMO (acceptance criterion 6),
// driven through the REAL crawl pipeline (CrawlOne: robots -> fetch -> extract -> persist
// -> M2 process -> rules -> alert pipeline -> mock Slack), mirroring realchange_e2e_test.go.
//
// Phase 1: serve valid Product JSON-LD (name + offers), crawl -> baseline, eligible, NO alert.
// Phase 2: redeploy with `offers` removed (the @type set is UNCHANGED), recrawl -> EXACTLY ONE
//
//	ingested alert event with ChangeType == "rich_result_product", severity critical, and a
//	Slack delivery.
//
// Phase 3: restore `offers`, recrawl -> the issue closes and NO further alert fires.
//
// Durable lesson (Round 4 e2e dedup): per-incident dedup keys on the incident's last-notified
// time and a DedupWindow, so the clock is ADVANCED between phases via the established fake-clock
// seam (mutable `now` behind a mutex) — otherwise a later phase's events could be silently
// swallowed inside the window and the proof would be vacuous.
func TestRichResultMarqueeMissingOffersE2E(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Mutable fake clock: advanced between phases so per-incident dedup never silently
	// swallows a later phase's notification inside the DedupWindow.
	var clockMu sync.Mutex
	cur := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	// Origin: serves robots + a Product page that drops `offers` when withOffers is false.
	var withOffers atomic.Bool
	withOffers.Store(true)
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
		_, _ = w.Write([]byte(productPage(origin.URL+"/", withOffers.Load())))
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
	if _, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: origin.URL, FirstSeen: clock(), NextCheckAt: clock(), Interval: 600, Importance: 1.0,
	}); err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

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
	deps := &recordingDeps{inner: &e2eDeps{store: st, engine: engine, pipeline: pipeline}}
	proc := NewProcessor(deps, diff.DefaultSimhashThreshold, clock)

	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(origin.Client(), "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:       clock,
		Processor: proc,
	}

	crawlNow := func(phase string) {
		u, err := st.GetURL(ctx, siteID, origin.URL)
		if err != nil {
			t.Fatalf("GetURL (%s): %v", phase, err)
		}
		if r := crawler.CrawlOne(ctx, u, 600, 86400, ""); r.Err != nil {
			t.Fatalf("CrawlOne (%s): %v", phase, r.Err)
		}
	}

	// Phase 1: baseline — valid Product (name + offers). Eligible, no alert.
	crawlNow("baseline")
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Fatalf("eligible baseline crawl must not alert; slackHits=%d", got)
	}
	urlRow, err := st.GetURL(ctx, siteID, origin.URL)
	if err != nil {
		t.Fatalf("GetURL after baseline: %v", err)
	}
	snap1, err := st.LatestSnapshot(ctx, urlRow.ID)
	if err != nil {
		t.Fatalf("LatestSnapshot baseline: %v", err)
	}
	// Sanity: extraction kept the Product block and it validates eligible — otherwise the
	// regression in phase 2 would be a false positive (proving nothing about lost eligibility).
	if snap1.JSONLD == "" || snap1.JSONLD == "null" {
		t.Fatalf("baseline snapshot must carry the Product JSON-LD, got %q", snap1.JSONLD)
	}

	// Phase 2: redeploy WITHOUT offers — eligibility lost, @type set unchanged.
	withOffers.Store(false)
	advance(time.Hour) // past the DedupWindow: a real recheck interval later

	before := len(deps.ingestedEvents())
	crawlNow("offers-removed")

	// Exactly one NEW ingested alert event this phase, and it is the marquee critical.
	phase2 := deps.ingestedEvents()[before:]
	var richCritical int
	var sawNonRich bool
	for _, e := range phase2 {
		if e.ChangeType == "rich_result_product" {
			richCritical++
			if e.Severity != model.SeverityCritical {
				t.Errorf("rich_result_product event severity = %q, want critical", e.Severity)
			}
		} else {
			// The @type set did NOT change, so no schema_types event should fire; any other
			// ingested change_type would mean the proof is leaking on an unrelated field.
			sawNonRich = true
			t.Logf("unexpected non-rich ingested event in phase 2: %+v", e)
		}
	}
	if richCritical != 1 {
		t.Fatalf("missing-offers regression must ingest EXACTLY ONE rich_result_product critical event, got %d (phase2 events %+v)", richCritical, phase2)
	}
	if sawNonRich {
		t.Errorf("the @type set is unchanged: no non-rich change_type should be ingested this phase; got %+v", phase2)
	}
	if got := atomic.LoadInt32(&slackHits); got != 1 {
		t.Fatalf("the marquee critical must deliver EXACTLY ONE Slack alert; slackHits=%d", got)
	}

	// The issue is open, queryable, critical, under its own rule_id.
	openIss, err := st.ListIssues(ctx, store.IssueFilter{URLID: &urlRow.ID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues after regression: %v", err)
	}
	foundCritical := false
	for _, iss := range openIss {
		if iss.RuleID == "rich_result_product" && iss.Severity == model.SeverityCritical {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Fatalf("expected an open critical rich_result_product issue after the offers drop, got %+v", openIss)
	}

	// Phase 3: restore offers — eligibility recovers, the issue closes, NO further alert.
	withOffers.Store(true)
	advance(time.Hour)

	slackBeforeRestore := atomic.LoadInt32(&slackHits)
	ingestedBeforeRestore := len(deps.ingestedEvents())
	crawlNow("offers-restored")

	if got := atomic.LoadInt32(&slackHits); got != slackBeforeRestore {
		t.Errorf("restoring offers must NOT fire a new Slack alert; slackHits went %d -> %d", slackBeforeRestore, got)
	}
	// No new rich_result_product alert event should be ingested on recovery (recovery is a
	// resolve, not a new finding).
	for _, e := range deps.ingestedEvents()[ingestedBeforeRestore:] {
		if e.ChangeType == "rich_result_product" {
			t.Errorf("recovery crawl must not ingest a new rich_result_product event; got %+v", e)
		}
	}

	openAfter, err := st.ListIssues(ctx, store.IssueFilter{URLID: &urlRow.ID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues after restore: %v", err)
	}
	for _, iss := range openAfter {
		if iss.RuleID == "rich_result_product" && iss.Status == model.IssueOpen {
			t.Errorf("rich_result_product issue must close once eligibility is restored, still open: %+v", iss)
		}
	}
}

// TestRichResultFirstCrawlOpensIssueNoSlack is acceptance criterion 7: a newly discovered
// URL whose Product markup is broken from its VERY FIRST observation (no prior baseline,
// oldSnap.ID == 0) must OPEN a queryable issue (tracked via `rabbot issues`) but emit NO
// Slack-bound event — the first-crawl guard at process.go:229 (only genuine fetch breakage,
// i.e. http_status, pages on a first crawl). Steady-state-invalid markup on a freshly
// discovered page is page hygiene, not a regression, so it must not page the operator.
//
// This is the no-baseline arm of the prior-baseline guard (Round 4 durable lesson: test
// BOTH arms). The has-baseline (critical-flip) arm is proven by the marquee e2e above.
func TestRichResultFirstCrawlOpensIssueNoSlack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Origin serves a page whose Product markup is broken (no offers/review/aggregateRating)
	// from the first crawl — steady-state ineligible, never previously eligible.
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
		_, _ = w.Write([]byte(productPage(origin.URL+"/", false))) // broken: no offers
	})
	origin = httptest.NewServer(mux)
	defer origin.Close()

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
	if _, err := st.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: origin.URL, FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1.0,
	}); err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

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
	deps := &recordingDeps{inner: &e2eDeps{store: st, engine: engine, pipeline: pipeline}}
	proc := NewProcessor(deps, diff.DefaultSimhashThreshold, clock)

	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(origin.Client(), "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:       clock,
		Processor: proc,
	}

	// First (and only) crawl of a freshly discovered URL: no prior snapshot exists.
	u, err := st.GetURL(ctx, siteID, origin.URL)
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	if r := crawler.CrawlOne(ctx, u, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne first crawl: %v", r.Err)
	}

	// NO Slack-bound event: the first-crawl guard keeps steady-state hygiene off Slack.
	if got := atomic.LoadInt32(&slackHits); got != 0 {
		t.Errorf("a first-crawl broken Product must NOT page Slack (steady-state hygiene, not a regression); slackHits=%d", got)
	}
	for _, e := range deps.ingestedEvents() {
		if e.ChangeType == "rich_result_product" {
			t.Errorf("a first-crawl broken Product must NOT bridge a Slack-bound rich_result_product event; got %+v", e)
		}
	}

	// BUT a queryable issue IS opened (tracked via `rabbot issues`).
	open, err := st.ListIssues(ctx, store.IssueFilter{URLID: &u.ID, OpenOnly: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	foundIssue := false
	var sev model.Severity
	for _, iss := range open {
		if iss.RuleID == "rich_result_product" {
			foundIssue = true
			sev = iss.Severity
		}
	}
	if !foundIssue {
		t.Fatalf("a first-crawl broken Product must OPEN a queryable rich_result_product issue, got %+v", open)
	}
	// First-crawl (Old.ID == 0) is never a LOST-eligibility flip, so the issue is warning-tier.
	if sev != model.SeverityWarning {
		t.Errorf("first-crawl ineligible Product issue must be WARNING (no prior baseline = not a lost-eligibility flip), got %q", sev)
	}
}
