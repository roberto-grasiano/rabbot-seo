package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// recordingGSCAPI records inspect + search-analytics calls so the registered job's
// task body can be observed end-to-end.
type recordingGSCAPI struct {
	mu        sync.Mutex
	saCalls   int
	inspected []string
}

func (a *recordingGSCAPI) SearchAnalyticsQuery(context.Context, string, gsc.SearchAnalyticsRequest) (*gsc.SearchAnalyticsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saCalls++
	return &gsc.SearchAnalyticsResponse{}, nil
}

func (a *recordingGSCAPI) InspectURL(_ context.Context, _ string, url string) (*gsc.InspectResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inspected = append(a.inspected, url)
	return &gsc.InspectResponse{}, nil
}

func (a *recordingGSCAPI) calls() (sa int, inspected []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.inspected))
	copy(out, a.inspected)
	return a.saCalls, out
}

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (string, error) { return "t", nil }
func (staticTokenProvider) Mode() string                          { return "service_account" }

// TestRegisterGSCPull_NilPullerRegistersNothing pins the disabled arm: a nil puller
// (no GSC-configured site) registers no job and reports registered=false.
func TestRegisterGSCPull_NilPullerRegistersNothing(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openCLITestStore(t)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	registered, err := registerGSCPull(context.Background(), logger, s, db, nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("registerGSCPull(nil): %v", err)
	}
	if registered {
		t.Fatal("nil puller must report registered=false")
	}
	if len(s.Jobs()) != 0 {
		t.Fatalf("nil puller must register no job; got %d jobs", len(s.Jobs()))
	}
}

// TestRegisterGSCPull_EnabledRegistersOneJob pins the enabled arm.
func TestRegisterGSCPull_EnabledRegistersOneJob(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openCLITestStore(t)
	p := &scheduler.GSCPuller{ResolveGSC: (&config.Config{}).GSCForBaseURL}

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	registered, err := registerGSCPull(context.Background(), logger, s, db, p, nil, time.Hour)
	if err != nil {
		t.Fatalf("registerGSCPull: %v", err)
	}
	if !registered {
		t.Fatal("non-nil puller must report registered=true")
	}
	if len(s.Jobs()) != 1 {
		t.Fatalf("enabled: want exactly 1 job, got %d", len(s.Jobs()))
	}
}

// TestRegisterGSCPull_TaskBodyPullsEnabledSkipsDisabled proves the registered job's
// TASK BODY iterates ListSites, pulls the enabled GSC-configured site, and skips a
// disabled one. We seed two sites (one enabled+GSC, one disabled+GSC) and observe
// the recording API: only the enabled site's property is pulled/inspected.
func TestRegisterGSCPull_TaskBodyPullsEnabledSkipsDisabled(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "gscjob.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	mkSite := func(base string, enabled bool) model.Site {
		id, aerr := db.AddSite(ctx, model.Site{
			BaseURL: base, Name: base, Enabled: enabled,
			MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
		})
		if aerr != nil {
			t.Fatalf("AddSite(%q): %v", base, aerr)
		}
		// one high-importance URL so the inspection pass has a candidate
		if _, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: id, URL: base + "page", FirstSeen: now, NextCheckAt: now,
			Interval: 600, Importance: 1.0, StatusType: model.StatusPage,
			LastFetchClass: model.FetchOK,
		}); uerr != nil {
			t.Fatalf("UpsertURL: %v", uerr)
		}
		st, gerr := db.GetSite(ctx, id)
		if gerr != nil {
			t.Fatalf("GetSite: %v", gerr)
		}
		return st
	}
	enabled := mkSite("https://on.test/", true)
	_ = mkSite("https://off.test/", false)

	api := &recordingGSCAPI{}
	cfg := &config.Config{Sites: []config.SiteConfig{
		{URL: "https://on.test/", GSC: config.GSCConfig{Property: "https://on.test/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"}},
		{URL: "https://off.test/", GSC: config.GSCConfig{Property: "https://off.test/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"}},
	}}
	p := &scheduler.GSCPuller{
		ResolveGSC:         cfg.GSCForBaseURL,
		Metrics:            db,
		Index:              db,
		Candidates:         &storeURLCandidates{db: db},
		API:                func(gsc.TokenProvider) (scheduler.GSCClient, error) { return api, nil },
		ProviderForSite:    func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) { return staticTokenProvider{}, nil },
		Now:                func() time.Time { return now },
		DailyInspectBudget: 10,
		Logger:             logger,
	}

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	if _, rerr := registerGSCPull(ctx, logger, s, db, p, nil, time.Hour); rerr != nil {
		t.Fatalf("registerGSCPull: %v", rerr)
	}
	s.Start()

	// WithStartImmediately fires the job on Start(); poll until the enabled site's
	// page has been inspected.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, inspected := api.calls()
		if len(inspected) > 0 {
			if inspected[0] != "https://on.test/page" {
				t.Fatalf("inspected %q, want the enabled site's page", inspected[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the GSC pull task body never ran within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The disabled site must never be inspected.
	_, inspected := api.calls()
	for _, u := range inspected {
		if u == "https://off.test/page" {
			t.Fatal("disabled site was pulled; the Enabled gate was not honored")
		}
	}
	_ = enabled
}

// recordingAlertSink is a GSCAlertSink that records Ingest/Resolve calls, so the
// wiring test can prove the registered job evaluates the W2 signals after a pull.
type recordingAlertSink struct {
	mu       sync.Mutex
	ingested []string // change_type:url
	resolved []string
}

func (r *recordingAlertSink) Ingest(_ context.Context, e alerts.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingested = append(r.ingested, e.ChangeType+":"+e.URL)
	return nil
}

func (r *recordingAlertSink) Resolve(_ context.Context, e alerts.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = append(r.resolved, e.ChangeType+":"+e.URL)
	return nil
}

// HasOpenMember reports no already-open membership: this single-tick wiring test only
// proves the registered job dispatches Evaluate (the signal fires on first sight). The
// fire-on-state-change idempotency across ticks is covered in gscsignals_test.go.
func (r *recordingAlertSink) HasOpenMember(_ context.Context, _ alerts.Event) (bool, error) {
	return false, nil
}

func (r *recordingAlertSink) snapshot() (ingested, resolved []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ingested...), append([]string(nil), r.resolved...)
}

// TestRegisterGSCPull_TaskBodyEvaluatesSignalsAfterPull proves the W2 evaluation_hook
// end-to-end through the registered job: a successful per-site Pull stores a
// url_index_status that disagrees with Rabbot's stored snapshot verdict, and the SAME
// task body then calls GSCSignals.Evaluate, which ingests an index_status_discrepancy
// through the (recording) pipeline. This is the integration contract — signals run
// inside the daily pull job, right after each site's state is refreshed.
func TestRegisterGSCPull_TaskBodyEvaluatesSignalsAfterPull(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "gscsig.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	const base = "https://sig.test/"
	const page = base + "page"
	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: base, Name: base, Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: page, FirstSeen: now, NextCheckAt: now,
		Interval: 600, Importance: 1.0, StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	// Rabbot's stored verdict: the page IS indexable.
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID: urlID, FetchedAt: now, HTTPStatus: 200, Indexable: true, ContentSHA256: "h",
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	site, err := db.GetSite(ctx, siteID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}

	// The mock inspect returns Google "Crawled - currently not indexed" → disagreement.
	api := &discrepancyAPI{coverage: "Crawled - currently not indexed"}
	cfg := &config.Config{Sites: []config.SiteConfig{
		{URL: base, GSC: config.GSCConfig{Property: base, Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"}},
	}}
	p := &scheduler.GSCPuller{
		ResolveGSC:         cfg.GSCForBaseURL,
		Metrics:            db,
		Index:              db,
		Candidates:         &storeURLCandidates{db: db},
		API:                func(gsc.TokenProvider) (scheduler.GSCClient, error) { return api, nil },
		ProviderForSite:    func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) { return staticTokenProvider{}, nil },
		Now:                func() time.Time { return now },
		DailyInspectBudget: 10,
		Logger:             logger,
	}
	sink := &recordingAlertSink{}
	signals := buildGSCSignals(db, sink)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	if _, rerr := registerGSCPull(ctx, logger, s, db, p, signals, time.Hour); rerr != nil {
		t.Fatalf("registerGSCPull: %v", rerr)
	}
	s.Start()

	// The wire-format change_type is the cross-package contract (the scheduler const
	// is unexported); assert against the literal the alert carries.
	want := "index_status_discrepancy:" + page
	deadline := time.Now().Add(5 * time.Second)
	for {
		ingested, _ := sink.snapshot()
		found := false
		for _, g := range ingested {
			if g == want {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the discrepancy was never ingested via the pull job within 5s; got %v", ingested)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = site
}

// TestGSCPuller_RealStoreRoundTrip proves the puller's writes land in Build1's REAL
// store and read back through the documented repo methods — the full W1 storage path
// (no fake store). It pulls one search-analytics day + one inspection via a mock API,
// then reads search_metrics + url_index_status back out.
func TestGSCPuller_RealStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://rt.test/", Name: "rt", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	if _, err := db.UpsertURL(ctx, model.URL{
		SiteID: siteID, URL: "https://rt.test/p", FirstSeen: now, NextCheckAt: now,
		Interval: 600, Importance: 1.0, StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	}); err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	site, err := db.GetSite(ctx, siteID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}

	crawl := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	api := &mockRoundTripAPI{
		sa: &gsc.SearchAnalyticsResponse{Rows: []gsc.SearchAnalyticsRow{
			{Keys: []string{"2026-06-15", "https://rt.test/p", "widget"}, Clicks: 5, Impressions: 120, CTR: 0.041, Position: 6.3},
		}},
		inspect: &gsc.InspectResponse{InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{
			Verdict: "PASS", CoverageState: "Submitted and indexed", IndexingState: "INDEXING_ALLOWED",
			GoogleCanonical: "https://rt.test/p", UserCanonical: "https://rt.test/p", LastCrawlTime: crawl,
		}}},
	}
	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://rt.test/",
		GSC: config.GSCConfig{Property: "https://rt.test/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"},
	}}}
	p := &scheduler.GSCPuller{
		ResolveGSC:         cfg.GSCForBaseURL,
		Metrics:            db,
		Index:              db,
		Candidates:         &storeURLCandidates{db: db},
		API:                func(gsc.TokenProvider) (scheduler.GSCClient, error) { return api, nil },
		ProviderForSite:    func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) { return staticTokenProvider{}, nil },
		Now:                func() time.Time { return now },
		DailyInspectBudget: 10,
	}
	if err := p.Pull(ctx, site); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// search_metrics round-trip.
	metrics, err := db.SearchMetricsForURL(ctx, siteID, "https://rt.test/p", time.Time{})
	if err != nil {
		t.Fatalf("SearchMetricsForURL: %v", err)
	}
	if len(metrics) != 1 {
		t.Fatalf("want 1 stored metric, got %d", len(metrics))
	}
	if metrics[0].Query != "widget" || metrics[0].Date != "2026-06-15" || metrics[0].Clicks != 5 || metrics[0].Impressions != 120 {
		t.Errorf("search metric not persisted faithfully: %+v", metrics[0])
	}

	// url_index_status round-trip.
	st, ok, err := db.LatestURLIndexStatus(ctx, siteID, "https://rt.test/p")
	if err != nil {
		t.Fatalf("LatestURLIndexStatus: %v", err)
	}
	if !ok {
		t.Fatal("expected a stored index status")
	}
	if st.Verdict != "PASS" || st.GoogleCanonical != "https://rt.test/p" {
		t.Errorf("index status not persisted faithfully: %+v", st)
	}
	if st.LastCrawlTime == nil || !st.LastCrawlTime.Equal(crawl) {
		t.Errorf("LastCrawlTime = %v, want %v (UTC round-trip)", st.LastCrawlTime, crawl)
	}
	if !st.InspectedAt.Equal(now) {
		t.Errorf("InspectedAt = %v, want the injected clock %v", st.InspectedAt, now)
	}
}

// mockRoundTripAPI returns fixed canned responses for the real-store round-trip.
type mockRoundTripAPI struct {
	sa      *gsc.SearchAnalyticsResponse
	inspect *gsc.InspectResponse
}

func (m *mockRoundTripAPI) SearchAnalyticsQuery(context.Context, string, gsc.SearchAnalyticsRequest) (*gsc.SearchAnalyticsResponse, error) {
	return m.sa, nil
}

func (m *mockRoundTripAPI) InspectURL(context.Context, string, string) (*gsc.InspectResponse, error) {
	return m.inspect, nil
}

// discrepancyAPI returns an inspect response whose coverageState is configurable, so
// the signal-evaluation wiring test can drive a Rabbot-vs-Google disagreement. The
// search-analytics half is an empty (no-op) response.
type discrepancyAPI struct {
	coverage string
	verdict  string
}

func (a *discrepancyAPI) SearchAnalyticsQuery(context.Context, string, gsc.SearchAnalyticsRequest) (*gsc.SearchAnalyticsResponse, error) {
	return &gsc.SearchAnalyticsResponse{}, nil
}

func (a *discrepancyAPI) InspectURL(context.Context, string, string) (*gsc.InspectResponse, error) {
	v := a.verdict
	if v == "" {
		v = "NEUTRAL"
	}
	return &gsc.InspectResponse{InspectionResult: gsc.InspectionResult{
		IndexStatusResult: gsc.IndexStatusResult{Verdict: v, CoverageState: a.coverage},
	}}, nil
}

// TestStoreURLCandidates_OrdersByImportance proves the production candidate adapter
// returns a site's URLs highest-importance-first and honors the limit.
func TestStoreURLCandidates_OrdersByImportance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "cand.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://c.test/", Name: "c", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	add := func(u string, imp float64) {
		if _, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: u, FirstSeen: now, NextCheckAt: now,
			Interval: 600, Importance: imp, StatusType: model.StatusPage,
			LastFetchClass: model.FetchOK,
		}); uerr != nil {
			t.Fatalf("UpsertURL(%q): %v", u, uerr)
		}
	}
	add("https://c.test/low", 0.1)
	add("https://c.test/high", 0.9)
	add("https://c.test/mid", 0.5)

	cs := &storeURLCandidates{db: db}
	got, err := cs.InspectionCandidates(ctx, siteID, 2)
	if err != nil {
		t.Fatalf("InspectionCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates (limit), got %d", len(got))
	}
	if got[0].URL != "https://c.test/high" || got[1].URL != "https://c.test/mid" {
		t.Fatalf("candidates not ordered by importance desc: %+v", got)
	}

	// A non-positive limit short-circuits to nothing (no query).
	if none, nerr := cs.InspectionCandidates(ctx, siteID, 0); nerr != nil || none != nil {
		t.Errorf("zero limit must return (nil, nil), got %+v / %v", none, nerr)
	}
}

// TestInspectionCandidates_QueryErrorSurfaced covers the query-error branch: querying
// a closed store surfaces a wrapped error rather than panicking.
func TestInspectionCandidates_QueryErrorSurfaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if cerr := db.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	cs := &storeURLCandidates{db: db}
	if _, qerr := cs.InspectionCandidates(ctx, 1, 5); qerr == nil {
		t.Error("querying a closed store must surface an error")
	}
}

// failingPullProvider is a ProviderForSite that always errors, so the registered job's
// per-site Pull fails and the error-logging branch is exercised.
func failingProviderPuller(db *store.DB, cfg *config.Config) *scheduler.GSCPuller {
	return &scheduler.GSCPuller{
		ResolveGSC: cfg.GSCForBaseURL,
		Metrics:    db,
		Index:      db,
		Candidates: &storeURLCandidates{db: db},
		API:        func(gsc.TokenProvider) (scheduler.GSCClient, error) { return &recordingGSCAPI{}, nil },
		ProviderForSite: func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) {
			return nil, errSentinelPull
		},
	}
}

var errSentinelPull = errTest("provider build always fails")

type errTest string

func (e errTest) Error() string { return string(e) }

// TestRegisterGSCPull_PerSitePullErrorIsLogged seeds one enabled GSC site and a puller
// whose Pull fails (provider build error); the registered job must log-and-continue
// (the per-site error branch) without crashing the scheduler.
func TestRegisterGSCPull_PerSitePullErrorIsLogged(t *testing.T) {
	t.Parallel()
	var logBuf safeBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "pullerr.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	id, aerr := db.AddSite(ctx, model.Site{
		BaseURL: "https://on.test/", Name: "on", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if aerr != nil {
		t.Fatalf("AddSite: %v", aerr)
	}
	if _, uerr := db.UpsertURL(ctx, model.URL{
		SiteID: id, URL: "https://on.test/p", FirstSeen: now, NextCheckAt: now,
		Interval: 600, Importance: 1.0, StatusType: model.StatusPage, LastFetchClass: model.FetchOK,
	}); uerr != nil {
		t.Fatalf("UpsertURL: %v", uerr)
	}

	cfg := &config.Config{Sites: []config.SiteConfig{{
		URL: "https://on.test/",
		GSC: config.GSCConfig{Property: "https://on.test/", Auth: config.GSCAuthServiceAccount, ServiceAccountKeyFile: "/k.json"},
	}}}
	p := failingProviderPuller(db, cfg)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	if _, rerr := registerGSCPull(ctx, logger, s, db, p, nil, time.Hour); rerr != nil {
		t.Fatalf("registerGSCPull: %v", rerr)
	}
	s.Start()

	// WithStartImmediately fires the job; poll until the failure is logged.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "gsc pull failed") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the per-site pull error was never logged:\n%s", logBuf.String())
}

// TestRegisterGSCPull_ListSitesErrorLogged covers the job body's ListSites-error
// branch: with the store closed before the job fires, ListSites fails and the error is
// logged (the task returns without iterating sites). The puller is non-nil so the job
// is registered.
func TestRegisterGSCPull_ListSitesErrorLogged(t *testing.T) {
	t.Parallel()
	var logBuf safeBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "listerr.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{}
	p := &scheduler.GSCPuller{ResolveGSC: cfg.GSCForBaseURL}

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	if _, rerr := registerGSCPull(ctx, logger, s, db, p, nil, time.Hour); rerr != nil {
		t.Fatalf("registerGSCPull: %v", rerr)
	}
	// Close the store so the immediate job's ListSites fails.
	if cerr := db.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	s.Start()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "list sites failed") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the ListSites failure was never logged:\n%s", logBuf.String())
}

// safeBuffer is a goroutine-safe bytes buffer for capturing async log output.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
