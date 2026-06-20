package mcpsrv

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// mockBridge is the canned, no-daemon Bridge implementation the handler tests run
// against. Each behaviour is injectable so a handler can be exercised against a
// healthy daemon, a down daemon, or a store error without any live process.
type mockBridge struct {
	healthErr error
	status    control.StatusResponse
	statusErr error
	sites     []SiteView
	sitesErr  error

	site       SiteDetail
	siteErr    error
	issues     []IssueView
	issuesErr  error
	history    HistoryView
	historyErr error

	// capture fields for arg assertions:
	lastIssueQuery   IssueQuery
	lastHistoryURL   string
	lastHistorySince time.Time

	// ── Phase 3 action results (canned) + capture fields ──
	addSiteResp control.AddSiteResponse
	addSiteErr  error
	lastAddSite control.AddSiteRequest

	crawlResp   control.CrawlResponse
	recheckErr  error
	lastRecheck string

	pauseErr  error
	resumeErr error

	ignoreErr    error
	lastIgnoreID int64

	testAlertErr     error
	lastTestNotifier string

	// set_config capture (Phase 4)
	setConfigErr       error
	setConfigCalls     int
	lastSetConfigKey   string
	lastSetConfigValue string

	// verify seam (Phase 5)
	verifyBegin      VerifyView
	verifyBeginErr   error
	verifyCheck      VerifyView
	verifyCheckErr   error
	lastVerifySiteID int64
	lastVerifyMethod string

	// report seam
	report          ReportView
	reportErr       error
	lastReportQuery ReportQuery

	// coverage seam (A2)
	coverage           CoverageView
	coverageErr        error
	lastCoverageSiteID int64

	// rich-results seam (A4)
	richResults        RichResultsView
	richResultsErr     error
	lastRichResultsURL string

	// health-score seam (A6)
	score           ScoreView
	scoreErr        error
	lastHealthQuery HealthQuery

	// link-graph seam (A9)
	links            LinksView
	linksErr         error
	lastBlastURL     string
	lastBlastLimit   int
	lastWhatLinksURL string
	lastWhatLinksLim int
	graph            GraphView
	graphErr         error
	lastGraphQuery   GraphQuery

	// GSC W2 read seams (index status + search performance).
	indexStatus         IndexStatusView
	indexStatusErr      error
	lastIndexStatusURL  string
	searchPerf          SearchPerformanceView
	searchPerfErr       error
	lastSearchPerfURL   string
	lastSearchPerfSince string
}

func (m *mockBridge) Health(context.Context) error { return m.healthErr }

func (m *mockBridge) Status(context.Context) (control.StatusResponse, error) {
	return m.status, m.statusErr
}

func (m *mockBridge) Sites(context.Context) ([]SiteView, error) {
	return m.sites, m.sitesErr
}

func (m *mockBridge) Site(context.Context, int64) (SiteDetail, error) {
	return m.site, m.siteErr
}

func (m *mockBridge) Issues(_ context.Context, q IssueQuery) ([]IssueView, error) {
	m.lastIssueQuery = q
	return m.issues, m.issuesErr
}

func (m *mockBridge) History(_ context.Context, url string, since time.Time) (HistoryView, error) {
	m.lastHistoryURL = url
	m.lastHistorySince = since
	return m.history, m.historyErr
}

func (m *mockBridge) AddSite(_ context.Context, req control.AddSiteRequest) (control.AddSiteResponse, error) {
	m.lastAddSite = req
	return m.addSiteResp, m.addSiteErr
}

func (m *mockBridge) Recheck(_ context.Context, target string) (control.CrawlResponse, error) {
	m.lastRecheck = target
	return m.crawlResp, m.recheckErr
}

func (m *mockBridge) Pause(context.Context) error  { return m.pauseErr }
func (m *mockBridge) Resume(context.Context) error { return m.resumeErr }

func (m *mockBridge) IgnoreIssue(_ context.Context, id int64) error {
	m.lastIgnoreID = id
	return m.ignoreErr
}

func (m *mockBridge) TestAlert(_ context.Context, notifier string) error {
	m.lastTestNotifier = notifier
	return m.testAlertErr
}

func (m *mockBridge) SetConfig(_ context.Context, key, value string) error {
	m.setConfigCalls++
	m.lastSetConfigKey = key
	m.lastSetConfigValue = value
	return m.setConfigErr
}

func (m *mockBridge) VerifyBegin(_ context.Context, siteID int64, method string) (VerifyView, error) {
	m.lastVerifySiteID = siteID
	m.lastVerifyMethod = method
	return m.verifyBegin, m.verifyBeginErr
}

func (m *mockBridge) VerifyCheck(_ context.Context, siteID int64, method string) (VerifyView, error) {
	m.lastVerifySiteID = siteID
	m.lastVerifyMethod = method
	return m.verifyCheck, m.verifyCheckErr
}

func (m *mockBridge) Report(_ context.Context, q ReportQuery) (ReportView, error) {
	m.lastReportQuery = q
	return m.report, m.reportErr
}

func (m *mockBridge) Coverage(_ context.Context, siteID int64) (CoverageView, error) {
	m.lastCoverageSiteID = siteID
	return m.coverage, m.coverageErr
}

func (m *mockBridge) RichResults(_ context.Context, url string) (RichResultsView, error) {
	m.lastRichResultsURL = url
	return m.richResults, m.richResultsErr
}

func (m *mockBridge) HealthScore(_ context.Context, q HealthQuery) (ScoreView, error) {
	m.lastHealthQuery = q
	return m.score, m.scoreErr
}

func (m *mockBridge) BlastRadius(_ context.Context, url string, limit int) (LinksView, error) {
	m.lastBlastURL = url
	m.lastBlastLimit = limit
	return m.links, m.linksErr
}

func (m *mockBridge) WhatLinksTo(_ context.Context, url string, limit int) (LinksView, error) {
	m.lastWhatLinksURL = url
	m.lastWhatLinksLim = limit
	return m.links, m.linksErr
}

func (m *mockBridge) GetLinkGraph(_ context.Context, q GraphQuery) (GraphView, error) {
	m.lastGraphQuery = q
	return m.graph, m.graphErr
}

func (m *mockBridge) IndexStatus(_ context.Context, url string) (IndexStatusView, error) {
	m.lastIndexStatusURL = url
	return m.indexStatus, m.indexStatusErr
}

func (m *mockBridge) SearchPerformance(_ context.Context, url, since string) (SearchPerformanceView, error) {
	m.lastSearchPerfURL = url
	m.lastSearchPerfSince = since
	return m.searchPerf, m.searchPerfErr
}

// Compile-time assertion that the mock (and thus the contract) satisfies Bridge.
var _ Bridge = (*mockBridge)(nil)

func TestBridgeHasActionMethods(t *testing.T) {
	t.Parallel()
	var b Bridge = (*mockBridge)(nil)
	// Compile-time surface check: these method values must exist on Bridge.
	_ = b.AddSite
	_ = b.Recheck
	_ = b.Pause
	_ = b.Resume
	_ = b.IgnoreIssue
	_ = b.TestAlert
}

func TestBridgeMockSatisfiesInterface(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	m := &mockBridge{
		healthErr: control.ErrDaemonNotRunning,
		status:    control.StatusResponse{Version: "1.2.3", SiteCount: 2, Paused: true},
		sites:     []SiteView{{ID: 7, URL: "https://example.com", Name: "Example", Enabled: true, VerificationState: "verified"}},
		sitesErr:  wantErr,
	}

	if err := m.Health(context.Background()); !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("Health() = %v, want ErrDaemonNotRunning", err)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() unexpected err: %v", err)
	}
	if st.Version != "1.2.3" || st.SiteCount != 2 || !st.Paused {
		t.Fatalf("Status() = %+v, not the canned value", st)
	}
	if _, err := m.Sites(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Sites() err = %v, want injected boom", err)
	}
}

func TestBridgeMockReadMethods(t *testing.T) {
	t.Parallel()

	wantSite := SiteDetail{ID: 5, URL: "https://acme.test", Name: "Acme", Enabled: true, VerificationState: "verified", OpenIssueCount: 2}
	wantIssues := []IssueView{{ID: 11, RuleID: "title-missing", Status: "open", Severity: "critical", ImpactPoints: 9}}
	wantHistory := HistoryView{URL: "https://acme.test/p", Changes: []ChangeView{{Field: "title", OldValue: "a", NewValue: "b", ChangeClass: "substantive"}}}

	m := &mockBridge{
		site:    wantSite,
		issues:  wantIssues,
		history: wantHistory,
	}

	got, err := m.Site(context.Background(), 5)
	if err != nil {
		t.Fatalf("Site() unexpected err: %v", err)
	}
	if !reflect.DeepEqual(got, wantSite) {
		t.Fatalf("Site() = %+v, want %+v", got, wantSite)
	}

	is, err := m.Issues(context.Background(), IssueQuery{})
	if err != nil {
		t.Fatalf("Issues() unexpected err: %v", err)
	}
	if len(is) != 1 || is[0].ID != 11 || is[0].Severity != "critical" {
		t.Fatalf("Issues() = %+v, want the canned issue", is)
	}

	h, err := m.History(context.Background(), "https://acme.test/p", time.Time{})
	if err != nil {
		t.Fatalf("History() unexpected err: %v", err)
	}
	if h.URL != "https://acme.test/p" || len(h.Changes) != 1 || h.Changes[0].Field != "title" {
		t.Fatalf("History() = %+v, want the canned history", h)
	}

	// Injected errors propagate.
	merr := &mockBridge{siteErr: control.ErrDaemonNotRunning, issuesErr: control.ErrDaemonNotRunning, historyErr: control.ErrDaemonNotRunning}
	if _, err := merr.Site(context.Background(), 1); !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("Site() err = %v, want ErrDaemonNotRunning", err)
	}
	if _, err := merr.Issues(context.Background(), IssueQuery{}); !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("Issues() err = %v, want ErrDaemonNotRunning", err)
	}
	if _, err := merr.History(context.Background(), "x", time.Time{}); !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("History() err = %v, want ErrDaemonNotRunning", err)
	}
}

func TestSiteViewFromModel(t *testing.T) {
	t.Parallel()

	s := model.Site{
		ID:      42,
		BaseURL: "https://acme.test",
		Name:    "Acme",
		Enabled: true,
	}
	// The verification state is now passed in from the authoritative proof record
	// (resolved by the production bridge), not read off model.Site.
	got := siteViewFromModel(s, "verified")
	want := SiteView{
		ID:                42,
		URL:               "https://acme.test",
		Name:              "Acme",
		Enabled:           true,
		VerificationState: "verified",
	}
	if got != want {
		t.Fatalf("siteViewFromModel() = %+v, want %+v", got, want)
	}
}

func TestSiteViewFromModel_DisabledThrottled(t *testing.T) {
	t.Parallel()

	s := model.Site{ID: 1, BaseURL: "https://x.test", Name: "", Enabled: false}
	got := siteViewFromModel(s, "throttled")
	if got.ID != 1 || got.URL != "https://x.test" || got.Enabled {
		t.Fatalf("siteViewFromModel() = %+v, did not preserve fields", got)
	}
	if got.VerificationState != "throttled" {
		t.Fatalf("VerificationState = %q, want %q", got.VerificationState, "throttled")
	}
}
