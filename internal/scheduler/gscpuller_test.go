package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/gsc"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// ── Test doubles ────────────────────────────────────────────────────────────

// fakeGSCAPI is an in-memory GSCClient. It records calls and returns canned
// responses (or a canned error) so the puller logic is exercised with no network.
type fakeGSCAPI struct {
	mu sync.Mutex

	saResp  *gsc.SearchAnalyticsResponse
	saErr   error
	saCalls []saCall

	inspect     map[string]*gsc.InspectResponse // keyed by inspection URL
	inspectErr  map[string]error                // per-URL error override
	inspectAll  error                           // error returned for every inspect (e.g. quota)
	inspectURLs []string                        // ordered record of inspected URLs
}

type saCall struct {
	property string
	req      gsc.SearchAnalyticsRequest
}

func (f *fakeGSCAPI) SearchAnalyticsQuery(_ context.Context, property string, req gsc.SearchAnalyticsRequest) (*gsc.SearchAnalyticsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saCalls = append(f.saCalls, saCall{property: property, req: req})
	if f.saErr != nil {
		return nil, f.saErr
	}
	if f.saResp != nil {
		return f.saResp, nil
	}
	return &gsc.SearchAnalyticsResponse{}, nil
}

func (f *fakeGSCAPI) InspectURL(_ context.Context, _ string, inspectionURL string) (*gsc.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectURLs = append(f.inspectURLs, inspectionURL)
	if f.inspectAll != nil {
		return nil, f.inspectAll
	}
	if f.inspectErr != nil {
		if err, ok := f.inspectErr[inspectionURL]; ok {
			return nil, err
		}
	}
	if f.inspect != nil {
		if r, ok := f.inspect[inspectionURL]; ok {
			return r, nil
		}
	}
	return &gsc.InspectResponse{}, nil
}

func (f *fakeGSCAPI) inspectedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.inspectURLs))
	copy(out, f.inspectURLs)
	return out
}

func (f *fakeGSCAPI) saRequests() []saCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]saCall, len(f.saCalls))
	copy(out, f.saCalls)
	return out
}

// fakeMetricsStore records SaveSearchMetrics batches.
type fakeMetricsStore struct {
	mu      sync.Mutex
	batches [][]model.SearchMetric
	err     error
}

func (s *fakeMetricsStore) SaveSearchMetrics(_ context.Context, m []model.SearchMetric) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	cp := make([]model.SearchMetric, len(m))
	copy(cp, m)
	s.batches = append(s.batches, cp)
	return nil
}

func (s *fakeMetricsStore) saved() []model.SearchMetric {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []model.SearchMetric
	for _, b := range s.batches {
		all = append(all, b...)
	}
	return all
}

// fakeIndexStore records UpsertURLIndexStatus calls.
type fakeIndexStore struct {
	mu       sync.Mutex
	upserts  []model.URLIndexStatus
	errOnURL map[string]error
}

func (s *fakeIndexStore) UpsertURLIndexStatus(_ context.Context, st model.URLIndexStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errOnURL != nil {
		if err, ok := s.errOnURL[st.URL]; ok {
			return err
		}
	}
	s.upserts = append(s.upserts, st)
	return nil
}

func (s *fakeIndexStore) upserted() []model.URLIndexStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.URLIndexStatus, len(s.upserts))
	copy(out, s.upserts)
	return out
}

// fakeCandidates returns a fixed candidate list, recording the requested limit.
type fakeCandidates struct {
	cands    []InspectCandidate
	err      error
	gotLimit int
	gotSite  int64
}

func (c *fakeCandidates) InspectionCandidates(_ context.Context, siteID int64, limit int) ([]InspectCandidate, error) {
	c.gotLimit = limit
	c.gotSite = siteID
	if c.err != nil {
		return nil, c.err
	}
	if len(c.cands) > limit {
		return c.cands[:limit], nil
	}
	return c.cands, nil
}

// staticProvider is a gsc.TokenProvider that yields a fixed token.
type staticProvider struct{ mode string }

func (p staticProvider) Token(context.Context) (string, error) { return "tok", nil }
func (p staticProvider) Mode() string {
	if p.mode == "" {
		return "service_account"
	}
	return p.mode
}

// newTestPuller builds a GSCPuller wired to the supplied doubles, with a fixed
// clock and a config that maps the test site's BaseURL to a service_account GSC
// block. The ProviderFactory returns a static token provider (the credential file
// read is mocked out).
func newTestPuller(t *testing.T, api GSCClient, ms SearchMetricsStore, is URLIndexStore, cs URLCandidateSource, clock time.Time) *GSCPuller {
	t.Helper()
	cfg := &config.Config{
		Sites: []config.SiteConfig{{
			URL: "https://ex.com/",
			GSC: config.GSCConfig{
				Property:              "https://ex.com/",
				Auth:                  config.GSCAuthServiceAccount,
				ServiceAccountKeyFile: "/does/not/matter.json",
			},
		}},
	}
	return &GSCPuller{
		ResolveGSC: cfg.GSCForBaseURL,
		Metrics:    ms,
		Index:      is,
		Candidates: cs,
		API:        func(gsc.TokenProvider) (GSCClient, error) { return api, nil },
		ProviderForSite: func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) {
			return staticProvider{}, nil
		},
		Now:                func() time.Time { return clock },
		DailyInspectBudget: 5,
	}
}

func testSite() model.Site {
	return model.Site{ID: 7, BaseURL: "https://ex.com/", Enabled: true}
}

// ── Search-analytics pull ───────────────────────────────────────────────────

func TestPullSearchAnalytics_StoresRowsAndUsesFinalDataState(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		saResp: &gsc.SearchAnalyticsResponse{
			Rows: []gsc.SearchAnalyticsRow{
				{Keys: []string{"2026-06-15", "https://ex.com/a", "boots"}, Clicks: 10, Impressions: 200, CTR: 0.05, Position: 4.2},
				{Keys: []string{"2026-06-16", "https://ex.com/b", "shoes"}, Clicks: 3, Impressions: 90, CTR: 0.033, Position: 8.0},
			},
		},
	}
	ms := &fakeMetricsStore{}
	p := newTestPuller(t, api, ms, &fakeIndexStore{}, &fakeCandidates{}, clock)

	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Fatalf("PullSearchAnalytics: %v", err)
	}

	reqs := api.saRequests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 searchAnalytics call, got %d", len(reqs))
	}
	r := reqs[0]
	if r.property != "https://ex.com/" {
		t.Errorf("property = %q, want the configured GSC property", r.property)
	}
	if r.req.DataState != "final" {
		t.Errorf("DataState = %q, want %q (respect the ~3-day partial-data lag)", r.req.DataState, "final")
	}
	// date must be requested so each row carries its true calendar day (the store grain).
	wantDims := []string{"date", "page", "query"}
	if strings.Join(r.req.Dimensions, ",") != strings.Join(wantDims, ",") {
		t.Errorf("Dimensions = %v, want %v", r.req.Dimensions, wantDims)
	}

	saved := ms.saved()
	if len(saved) != 2 {
		t.Fatalf("want 2 metrics stored, got %d", len(saved))
	}
	got := saved[0]
	if got.SiteID != 7 {
		t.Errorf("SiteID = %d, want 7", got.SiteID)
	}
	if got.URL != "https://ex.com/a" || got.Query != "boots" {
		t.Errorf("row0 url/query = %q/%q, want page/query keys mapped", got.URL, got.Query)
	}
	if got.Date != "2026-06-15" {
		t.Errorf("row0 date = %q, want the row's own day 2026-06-15", got.Date)
	}
	if got.Clicks != 10 || got.Impressions != 200 {
		t.Errorf("row0 clicks/impressions = %d/%d, want 10/200 (float→int)", got.Clicks, got.Impressions)
	}
	if got.CTR != 0.05 || got.Position != 4.2 {
		t.Errorf("row0 ctr/position = %v/%v, want 0.05/4.2", got.CTR, got.Position)
	}
}

func TestPullSearchAnalytics_DateWindowExcludesPartialDays(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{saResp: &gsc.SearchAnalyticsResponse{}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, &fakeCandidates{}, clock)
	p.LookbackDays = 7

	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Fatalf("PullSearchAnalytics: %v", err)
	}
	reqs := api.saRequests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 call, got %d", len(reqs))
	}
	// End date must be at least 3 days behind "today" (the partial-data lag), and
	// the window must be LookbackDays wide ending at that final day.
	end, err := time.Parse("2006-01-02", reqs[0].req.EndDate)
	if err != nil {
		t.Fatalf("EndDate %q not a YYYY-MM-DD date: %v", reqs[0].req.EndDate, err)
	}
	wantEnd := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) // 2026-06-19 minus 3 days
	if !end.Equal(wantEnd) {
		t.Errorf("EndDate = %s, want %s (today − 3 partial days)", reqs[0].req.EndDate, wantEnd.Format("2006-01-02"))
	}
	start, err := time.Parse("2006-01-02", reqs[0].req.StartDate)
	if err != nil {
		t.Fatalf("StartDate %q not a YYYY-MM-DD date: %v", reqs[0].req.StartDate, err)
	}
	wantStart := wantEnd.AddDate(0, 0, -6) // 7-day inclusive window
	if !start.Equal(wantStart) {
		t.Errorf("StartDate = %s, want %s (LookbackDays inclusive window)", reqs[0].req.StartDate, wantStart.Format("2006-01-02"))
	}
}

func TestPullSearchAnalytics_EmptyResponseIsCleanNoStore(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{saResp: &gsc.SearchAnalyticsResponse{}}
	ms := &fakeMetricsStore{}
	p := newTestPuller(t, api, ms, &fakeIndexStore{}, &fakeCandidates{}, clock)
	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Fatalf("PullSearchAnalytics: %v", err)
	}
	if got := ms.saved(); len(got) != 0 {
		t.Fatalf("empty response must store nothing, got %d rows", len(got))
	}
}

func TestPullSearchAnalytics_RowMissingPageKeyIsSkipped(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		saResp: &gsc.SearchAnalyticsResponse{
			Rows: []gsc.SearchAnalyticsRow{
				{Keys: []string{"2026-06-15", "", "q"}, Clicks: 1},                  // no page → skip
				{Keys: []string{"2026-06-15", "https://ex.com/ok", "q"}, Clicks: 2}, // ok
				{Keys: []string{"", "https://ex.com/nodate", "q"}, Clicks: 3},       // no date → skip
			},
		},
	}
	ms := &fakeMetricsStore{}
	p := newTestPuller(t, api, ms, &fakeIndexStore{}, &fakeCandidates{}, clock)
	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Fatalf("PullSearchAnalytics: %v", err)
	}
	saved := ms.saved()
	if len(saved) != 1 || saved[0].URL != "https://ex.com/ok" {
		t.Fatalf("want only the row with both a page AND date key stored, got %+v", saved)
	}
}

func TestPullSearchAnalytics_APIErrorPropagates(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	sentinel := errors.New("boom")
	api := &fakeGSCAPI{saErr: sentinel}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, &fakeCandidates{}, clock)
	err := p.PullSearchAnalytics(context.Background(), testSite())
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the API error to propagate, got %v", err)
	}
}

// ── URL-inspection pass ─────────────────────────────────────────────────────

func TestRunInspectionPass_InspectsCandidatesAndUpserts(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	crawl := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/p1": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{
				Verdict: "PASS", CoverageState: "Submitted and indexed", IndexingState: "INDEXING_ALLOWED",
				GoogleCanonical: "https://ex.com/p1", UserCanonical: "https://ex.com/p1",
				RobotsTxtState: "ALLOWED", PageFetchState: "SUCCESSFUL", CrawledAs: "MOBILE",
				LastCrawlTime: crawl,
			}}},
			"https://ex.com/p2": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{
				Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed",
			}}},
		},
	}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{
		{URL: "https://ex.com/p1", Importance: 0.9},
		{URL: "https://ex.com/p2", Importance: 0.8},
	}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)

	n, err := p.RunInspectionPass(context.Background(), testSite(), 5)
	if err != nil {
		t.Fatalf("RunInspectionPass: %v", err)
	}
	if n != 2 {
		t.Fatalf("inspected count = %d, want 2", n)
	}
	up := is.upserted()
	if len(up) != 2 {
		t.Fatalf("want 2 upserts, got %d", len(up))
	}
	first := up[0]
	if first.SiteID != 7 || first.URL != "https://ex.com/p1" {
		t.Errorf("row0 site/url = %d/%q, want 7/https://ex.com/p1", first.SiteID, first.URL)
	}
	if first.Verdict != "PASS" || first.CoverageState != "Submitted and indexed" {
		t.Errorf("row0 verdict/coverage = %q/%q", first.Verdict, first.CoverageState)
	}
	if first.GoogleCanonical != "https://ex.com/p1" || first.UserCanonical != "https://ex.com/p1" {
		t.Errorf("row0 canonicals not mapped: %+v", first)
	}
	if first.LastCrawlTime == nil || !first.LastCrawlTime.Equal(crawl) {
		t.Errorf("row0 LastCrawlTime = %v, want %v", first.LastCrawlTime, crawl)
	}
	if !first.InspectedAt.Equal(clock) {
		t.Errorf("row0 InspectedAt = %v, want clock %v", first.InspectedAt, clock)
	}
}

func TestRunInspectionPass_NeverCrawledLeavesLastCrawlNil(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/new": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{
				Verdict: "NEUTRAL", // LastCrawlTime zero → nil
			}}},
		},
	}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/new", Importance: 0.5}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)
	if _, err := p.RunInspectionPass(context.Background(), testSite(), 5); err != nil {
		t.Fatalf("RunInspectionPass: %v", err)
	}
	up := is.upserted()
	if len(up) != 1 {
		t.Fatalf("want 1 upsert, got %d", len(up))
	}
	if up[0].LastCrawlTime != nil {
		t.Errorf("a never-crawled URL must leave LastCrawlTime nil, got %v", up[0].LastCrawlTime)
	}
}

func TestRunInspectionPass_BudgetCapsInspections(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{}
	cs := &fakeCandidates{cands: []InspectCandidate{
		{URL: "https://ex.com/1"}, {URL: "https://ex.com/2"}, {URL: "https://ex.com/3"},
		{URL: "https://ex.com/4"}, {URL: "https://ex.com/5"},
	}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, cs, clock)

	n, err := p.RunInspectionPass(context.Background(), testSite(), 2)
	if err != nil {
		t.Fatalf("RunInspectionPass: %v", err)
	}
	if n != 2 {
		t.Fatalf("inspected = %d, want capped at budget 2", n)
	}
	// The candidate source must be asked for at most `budget` URLs.
	if cs.gotLimit != 2 {
		t.Errorf("candidate limit requested = %d, want 2 (budget)", cs.gotLimit)
	}
	if got := len(api.inspectedURLs()); got != 2 {
		t.Errorf("inspected %d URLs, want 2", got)
	}
}

func TestRunInspectionPass_ZeroBudgetInspectsNothing(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/1"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, cs, clock)
	n, err := p.RunInspectionPass(context.Background(), testSite(), 0)
	if err != nil {
		t.Fatalf("RunInspectionPass: %v", err)
	}
	if n != 0 {
		t.Fatalf("inspected = %d, want 0 on a zero budget", n)
	}
	if got := len(api.inspectedURLs()); got != 0 {
		t.Errorf("inspected %d URLs on zero budget, want 0", got)
	}
}

func TestRunInspectionPass_QuotaErrorStopsPassNoError(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// Every inspect returns a quota error; the pass must stop early and NOT bubble
	// the quota error (it is an expected budget condition, not a pull failure).
	quota := &gsc.APIError{HTTPStatus: http.StatusTooManyRequests, Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota"}
	api := &fakeGSCAPI{inspectAll: quota}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/1"}, {URL: "https://ex.com/2"}, {URL: "https://ex.com/3"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)

	n, err := p.RunInspectionPass(context.Background(), testSite(), 5)
	if err != nil {
		t.Fatalf("a quota error must not fail the pass, got %v", err)
	}
	if n != 0 {
		t.Errorf("no upserts on a quota-throttled pass, got n=%d", n)
	}
	// It must stop at the FIRST quota error rather than burning the whole budget.
	if got := len(api.inspectedURLs()); got != 1 {
		t.Errorf("quota must halt the pass after the first 429, inspected %d", got)
	}
}

func TestRunInspectionPass_PerURLErrorSkipsAndContinues(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// A permanent per-URL API error (e.g. 404 for a URL not in the property) must
	// be skipped; the pass continues to the next candidate.
	notFound := &gsc.APIError{HTTPStatus: http.StatusNotFound, Code: 404, Message: "not found"}
	api := &fakeGSCAPI{
		inspectErr: map[string]error{"https://ex.com/bad": notFound},
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/good": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{Verdict: "PASS"}}},
		},
	}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/bad"}, {URL: "https://ex.com/good"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)

	n, err := p.RunInspectionPass(context.Background(), testSite(), 5)
	if err != nil {
		t.Fatalf("RunInspectionPass: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 successful upsert (bad skipped), got %d", n)
	}
	if got := is.upserted(); len(got) != 1 || got[0].URL != "https://ex.com/good" {
		t.Fatalf("only the good URL should be stored, got %+v", got)
	}
}

// ── Pull orchestration / severability ───────────────────────────────────────

func TestPull_RunsBothPullsForConfiguredSite(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		saResp: &gsc.SearchAnalyticsResponse{Rows: []gsc.SearchAnalyticsRow{
			{Keys: []string{"2026-06-15", "https://ex.com/a", "q"}, Clicks: 1},
		}},
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/a": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{Verdict: "PASS"}}},
		},
	}
	ms := &fakeMetricsStore{}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/a"}}}
	p := newTestPuller(t, api, ms, is, cs, clock)

	if err := p.Pull(context.Background(), testSite()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(ms.saved()) != 1 {
		t.Errorf("search-analytics not pulled: %d metrics", len(ms.saved()))
	}
	if len(is.upserted()) != 1 {
		t.Errorf("inspection not run: %d statuses", len(is.upserted()))
	}
}

func TestPull_SkipsSiteWithoutGSCConfig(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, &fakeCandidates{}, clock)

	other := model.Site{ID: 99, BaseURL: "https://other.example/", Enabled: true}
	if err := p.Pull(context.Background(), other); err != nil {
		t.Fatalf("Pull of an unconfigured site must be a clean no-op, got %v", err)
	}
	if len(api.saRequests()) != 0 || len(api.inspectedURLs()) != 0 {
		t.Errorf("an unconfigured site must make no GSC calls")
	}
}

func TestPull_SearchAnalyticsFailureDoesNotBlockInspection(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		saErr: errors.New("search-analytics down"),
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/a": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{Verdict: "PASS"}}},
		},
	}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/a"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)

	err := p.Pull(context.Background(), testSite())
	// Pull joins errors but still attempts both halves: the inspection must run.
	if err == nil {
		t.Fatalf("Pull should surface the search-analytics error")
	}
	if len(is.upserted()) != 1 {
		t.Errorf("inspection must still run despite the SA failure: %d upserts", len(is.upserted()))
	}
}

func TestPull_ProviderBuildFailureIsSurfacedNotPanic(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	p := newTestPuller(t, &fakeGSCAPI{}, &fakeMetricsStore{}, &fakeIndexStore{}, &fakeCandidates{}, clock)
	p.ProviderForSite = func(context.Context, config.GSCConfig) (gsc.TokenProvider, error) {
		return nil, errors.New("key file unreadable")
	}
	err := p.Pull(context.Background(), testSite())
	if err == nil || !strings.Contains(err.Error(), "key file unreadable") {
		t.Fatalf("a provider build failure must be surfaced, got %v", err)
	}
}

// ── Defaults, helpers & error branches ──────────────────────────────────────

// TestPuller_DefaultClockAndBudget covers the nil-Now (defaults to time.Now().UTC())
// and nil-DailyInspectBudget (defaults to gscDefaultDailyInspectBudget) helper arms.
func TestPuller_DefaultClockAndBudget(t *testing.T) {
	t.Parallel()
	p := &GSCPuller{} // no Now, no DailyInspectBudget, no LookbackDays

	got := p.now()
	if got.IsZero() || got.Location() != time.UTC {
		t.Errorf("default now() = %v, want a non-zero UTC time", got)
	}
	if b := p.inspectBudget(); b != gscDefaultDailyInspectBudget {
		t.Errorf("default inspectBudget() = %d, want %d", b, gscDefaultDailyInspectBudget)
	}
	if d := p.lookbackDays(); d != gscDefaultLookbackDays {
		t.Errorf("default lookbackDays() = %d, want %d", d, gscDefaultLookbackDays)
	}
}

// TestSiteContext_NilResolverIsCleanNoOp covers siteContext's nil-ResolveGSC arm: a
// puller with no resolver treats every site as not-GSC (ok=false, no error).
func TestSiteContext_NilResolverIsCleanNoOp(t *testing.T) {
	t.Parallel()
	p := &GSCPuller{} // ResolveGSC nil
	if err := p.Pull(context.Background(), testSite()); err != nil {
		t.Fatalf("nil resolver Pull must be a clean no-op, got %v", err)
	}
	// The split entry points are no-ops too.
	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Errorf("PullSearchAnalytics no-op: %v", err)
	}
	if n, err := p.RunInspectionPass(context.Background(), testSite(), 5); err != nil || n != 0 {
		t.Errorf("RunInspectionPass no-op: n=%d err=%v", n, err)
	}
}

// TestSiteContext_APIClientBuildErrorSurfaced covers the client-build error arm: a
// failing API factory makes siteContext (and thus Pull) error.
func TestSiteContext_APIClientBuildErrorSurfaced(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	p := newTestPuller(t, &fakeGSCAPI{}, &fakeMetricsStore{}, &fakeIndexStore{}, &fakeCandidates{}, clock)
	p.API = func(gsc.TokenProvider) (GSCClient, error) { return nil, errors.New("client build failed") }
	if err := p.Pull(context.Background(), testSite()); err == nil || !strings.Contains(err.Error(), "build client") {
		t.Fatalf("a client-build failure must be surfaced, got %v", err)
	}
}

// TestRunInspectionPass_CandidateErrorSurfaced covers the candidate-source error arm.
func TestRunInspectionPass_CandidateErrorSurfaced(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	cs := &fakeCandidates{err: errors.New("candidate query failed")}
	p := newTestPuller(t, &fakeGSCAPI{}, &fakeMetricsStore{}, &fakeIndexStore{}, cs, clock)
	if _, err := p.RunInspectionPass(context.Background(), testSite(), 5); err == nil ||
		!strings.Contains(err.Error(), "select inspection candidates") {
		t.Fatalf("a candidate-source error must be surfaced, got %v", err)
	}
}

// TestRunInspectionPass_StoreWriteErrorSurfaced covers the upsert-error arm: a store
// write failure is SYSTEMIC and must be surfaced (joined), unlike a per-URL API error.
func TestRunInspectionPass_StoreWriteErrorSurfaced(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{
		inspect: map[string]*gsc.InspectResponse{
			"https://ex.com/p": {InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{Verdict: "PASS"}}},
		},
	}
	is := &fakeIndexStore{errOnURL: map[string]error{"https://ex.com/p": errors.New("disk full")}}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/p"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, is, cs, clock)
	n, err := p.RunInspectionPass(context.Background(), testSite(), 5)
	if err == nil || !strings.Contains(err.Error(), "store index status") {
		t.Fatalf("a store-write error must be surfaced, got %v", err)
	}
	if n != 0 {
		t.Errorf("a failed upsert must not count as stored, got n=%d", n)
	}
}

// TestRunInspectionPass_CtxCancelStopsPass covers the cooperative-cancellation arm: a
// cancelled context halts the pass before inspecting.
func TestRunInspectionPass_CtxCancelStopsPass(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/1"}, {URL: "https://ex.com/2"}}}
	p := newTestPuller(t, api, &fakeMetricsStore{}, &fakeIndexStore{}, cs, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := p.RunInspectionPass(ctx, testSite(), 5)
	if err != nil {
		t.Fatalf("a cancelled pass should stop cleanly, got %v", err)
	}
	if n != 0 || len(api.inspectedURLs()) != 0 {
		t.Errorf("a cancelled context must inspect nothing: n=%d inspected=%d", n, len(api.inspectedURLs()))
	}
}

// TestPullSearchAnalytics_StoreErrorSurfaced covers the SaveSearchMetrics error arm.
func TestPullSearchAnalytics_StoreErrorSurfaced(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{saResp: &gsc.SearchAnalyticsResponse{Rows: []gsc.SearchAnalyticsRow{
		{Keys: []string{"2026-06-15", "https://ex.com/a", "q"}, Clicks: 1},
	}}}
	ms := &fakeMetricsStore{err: errors.New("store down")}
	p := newTestPuller(t, api, ms, &fakeIndexStore{}, &fakeCandidates{}, clock)
	if err := p.PullSearchAnalytics(context.Background(), testSite()); err == nil ||
		!strings.Contains(err.Error(), "store search metrics") {
		t.Fatalf("a SaveSearchMetrics error must be surfaced, got %v", err)
	}
}

// TestPullSearchAnalytics_AllRowsSkippedStoresNothing covers the "every row missing a
// key → metrics empty → no store call" arm (distinct from an empty response).
func TestPullSearchAnalytics_AllRowsSkippedStoresNothing(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	api := &fakeGSCAPI{saResp: &gsc.SearchAnalyticsResponse{Rows: []gsc.SearchAnalyticsRow{
		{Keys: []string{"2026-06-15", "", "q"}}, // no page → skipped
	}}}
	ms := &fakeMetricsStore{}
	p := newTestPuller(t, api, ms, &fakeIndexStore{}, &fakeCandidates{}, clock)
	if err := p.PullSearchAnalytics(context.Background(), testSite()); err != nil {
		t.Fatalf("PullSearchAnalytics: %v", err)
	}
	if len(ms.saved()) != 0 {
		t.Errorf("all rows skipped must store nothing, got %d", len(ms.saved()))
	}
}

// TestIsQuotaError_NonAPIIsFalse covers isQuotaError's non-APIError arm (a plain error
// is not a quota signal, so the pass must NOT treat it as a budget stop).
func TestIsQuotaError_NonAPIIsFalse(t *testing.T) {
	t.Parallel()
	if isQuotaError(errors.New("some non-API error")) {
		t.Error("a non-APIError must not be treated as a quota error")
	}
	if isQuotaError(&gsc.APIError{HTTPStatus: 404, Code: 404}) {
		t.Error("a 404 is not a quota error")
	}
	if !isQuotaError(&gsc.APIError{HTTPStatus: 429, Code: 429, Status: "RESOURCE_EXHAUSTED"}) {
		t.Error("a 429 RESOURCE_EXHAUSTED must be a quota error")
	}
}

// TestLogInspectSkip_NilLoggerAndCancelledCtxAreSilent covers logInspectSkip's guard
// arms (nil logger, cancelled ctx) — both must be silent no-ops that never panic.
func TestLogInspectSkip_GuardsAreSilent(t *testing.T) {
	t.Parallel()
	// Nil logger: no-op.
	p := &GSCPuller{}
	p.logInspectSkip(context.Background(), testSite(), "https://ex.com/x", errors.New("e"))

	// Logger set but ctx cancelled: still silent.
	var buf strings.Builder
	p2 := &GSCPuller{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p2.logInspectSkip(ctx, testSite(), "https://ex.com/x", errors.New("e"))
	if buf.Len() != 0 {
		t.Errorf("a cancelled ctx must suppress the skip log, got %q", buf.String())
	}

	// Logger set, live ctx: it logs (the non-guard arm), and the URL is present.
	var buf2 strings.Builder
	p3 := &GSCPuller{Logger: slog.New(slog.NewTextHandler(&buf2, nil))}
	p3.logInspectSkip(context.Background(), testSite(), "https://ex.com/skipme", errors.New("boom"))
	if !strings.Contains(buf2.String(), "https://ex.com/skipme") {
		t.Errorf("a live-ctx skip must log the URL, got %q", buf2.String())
	}
}

// ── Integration: real gsc.Client over httptest (canned googleapis JSON) ──────

func TestPuller_AgainstRealClientHTTPTest(t *testing.T) {
	clock := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)

	// A single httptest server serving both the webmasters and inspection paths
	// with canned googleapis JSON (no live credentials).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/searchAnalytics/query"):
			_ = json.NewEncoder(w).Encode(gsc.SearchAnalyticsResponse{
				Rows: []gsc.SearchAnalyticsRow{
					{Keys: []string{"2026-06-15", "https://ex.com/a", "boots"}, Clicks: 12, Impressions: 300, CTR: 0.04, Position: 3.1},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/urlInspection/index:inspect"):
			_ = json.NewEncoder(w).Encode(gsc.InspectResponse{
				InspectionResult: gsc.InspectionResult{IndexStatusResult: gsc.IndexStatusResult{
					Verdict: "PASS", CoverageState: "Submitted and indexed",
					GoogleCanonical: "https://ex.com/a", UserCanonical: "https://ex.com/a",
				}},
			})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	realClient, err := gsc.NewClient(gsc.Options{
		Token:          staticProvider{},
		HTTPClient:     srv.Client(),
		BaseURL:        srv.URL,
		InspectBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("gsc.NewClient: %v", err)
	}

	ms := &fakeMetricsStore{}
	is := &fakeIndexStore{}
	cs := &fakeCandidates{cands: []InspectCandidate{{URL: "https://ex.com/a", Importance: 0.9}}}
	p := newTestPuller(t, realClient, ms, is, cs, clock)

	if err := p.Pull(context.Background(), testSite()); err != nil {
		t.Fatalf("Pull against real client: %v", err)
	}
	if len(ms.saved()) != 1 || ms.saved()[0].Clicks != 12 {
		t.Errorf("search metrics not round-tripped through the real client: %+v", ms.saved())
	}
	if len(is.upserted()) != 1 || is.upserted()[0].Verdict != "PASS" {
		t.Errorf("index status not round-tripped through the real client: %+v", is.upserted())
	}
}
