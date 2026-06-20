package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// newClientAgainst wires a Client against a test server that always requires the
// "tok" bearer token, mirroring the M1 client tests.
func newClientAgainst(t *testing.T, hooks Hooks) *Client {
	t.Helper()
	srv := NewServer(ServerOptions{Token: "tok", Version: "0.1.0", Hooks: hooks})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return NewClientWithBaseURL(ts.URL, "tok")
}

func TestClientSites(t *testing.T) {
	c := newClientAgainst(t, Hooks{
		ListSites: func(context.Context) ([]SiteSummary, error) {
			return []SiteSummary{{ID: 1, URL: "https://a.test", VerificationState: "verified"}}, nil
		},
	})
	got, err := c.Sites(context.Background())
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].VerificationState != "verified" {
		t.Fatalf("Sites = %+v", got)
	}
}

func TestClientSiteDetail(t *testing.T) {
	c := newClientAgainst(t, Hooks{
		SiteDetail: func(_ context.Context, id int64) (SiteDetailResponse, bool, error) {
			return SiteDetailResponse{ID: id, URL: "https://a.test", OpenIssueCount: 2}, true, nil
		},
	})
	got, err := c.SiteDetail(context.Background(), 9)
	if err != nil {
		t.Fatalf("SiteDetail: %v", err)
	}
	if got.ID != 9 || got.OpenIssueCount != 2 {
		t.Fatalf("SiteDetail = %+v", got)
	}
}

func TestClientSiteDetailNotFound(t *testing.T) {
	c := newClientAgainst(t, Hooks{
		SiteDetail: func(context.Context, int64) (SiteDetailResponse, bool, error) {
			return SiteDetailResponse{}, false, nil
		},
	})
	// Not-found is HTTP 200 with not_found:true; the client decodes it into
	// SiteDetailResponse (all-zero) and a separate found indicator.
	got, found, err := c.SiteDetailFound(context.Background(), 404)
	if err != nil {
		t.Fatalf("SiteDetailFound: %v", err)
	}
	if found {
		t.Fatalf("want found=false for unknown id, got %+v", got)
	}
}

func TestClientIssuesBuildsQuery(t *testing.T) {
	var gotQuery IssueQuery
	c := newClientAgainst(t, Hooks{
		Issues: func(_ context.Context, q IssueQuery) ([]IssueView, error) {
			gotQuery = q
			return []IssueView{{ID: 1, RuleID: "r"}}, nil
		},
	})
	sid := int64(42)
	got, err := c.Issues(context.Background(), &sid, "critical", "open", "")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Issues len = %d", len(got))
	}
	if gotQuery.SiteID == nil || *gotQuery.SiteID != 42 || gotQuery.Severity != "critical" || gotQuery.Status != "open" {
		t.Fatalf("server saw query %+v", gotQuery)
	}
}

func TestClientHistory(t *testing.T) {
	var gotSince time.Time
	c := newClientAgainst(t, Hooks{
		History: func(_ context.Context, u string, since time.Time) (HistoryResponse, error) {
			gotSince = since
			return HistoryResponse{URL: u, Changes: []ChangeView{{Field: "title"}}}, nil
		},
	})
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := c.History(context.Background(), "https://a.test/", since)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if got.URL != "https://a.test/" || len(got.Changes) != 1 {
		t.Fatalf("History = %+v", got)
	}
	if !gotSince.Equal(since) {
		t.Fatalf("server since = %v, want %v", gotSince, since)
	}
}

func TestClientRichResults_RoundTrip(t *testing.T) {
	t.Parallel()
	var gotURL string
	c := newClientAgainst(t, Hooks{
		RichResults: func(_ context.Context, u string) (RichResultsResponse, error) {
			gotURL = u
			return RichResultsResponse{
				URL:         u,
				HasSnapshot: true,
				Profile:     "grr-2026.06",
				Entities:    []RichResultEntity{{Type: "Article", RawType: "BlogPosting", Eligible: false, Missing: []string{"headline"}}},
				Unprofiled:  2,
			}, nil
		},
	})
	got, err := c.RichResults(context.Background(), "https://a.test/post")
	if err != nil {
		t.Fatalf("RichResults: %v", err)
	}
	if gotURL != "https://a.test/post" {
		t.Fatalf("server url = %q, want https://a.test/post", gotURL)
	}
	if got.Profile != "grr-2026.06" || !got.HasSnapshot || got.Unprofiled != 2 {
		t.Fatalf("RichResults = %+v", got)
	}
	if len(got.Entities) != 1 || got.Entities[0].RawType != "BlogPosting" || got.Entities[0].Missing[0] != "headline" {
		t.Fatalf("entities = %+v", got.Entities)
	}
}

func TestClientRichResults_NotFoundIsData(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		RichResults: func(_ context.Context, u string) (RichResultsResponse, error) {
			return RichResultsResponse{URL: u, NotFound: true}, nil
		},
	})
	got, err := c.RichResults(context.Background(), "https://a.test/missing")
	if err != nil {
		t.Fatalf("RichResults not-found must be data, not error: %v", err)
	}
	if !got.NotFound {
		t.Fatalf("want not_found=true, got %+v", got)
	}
}

func TestClientReport_RoundTrip(t *testing.T) {
	t.Parallel()
	// Capture what the server actually parsed off the query string, proving the
	// CLIENT encoded since/site_id/top correctly (the handler parse is tested
	// separately in TestHandleReport_OK against a hand-written URL).
	var (
		gotSince  time.Time
		gotSiteID *int64
		gotTop    int
	)
	ts := newTestServer(Hooks{
		Report: func(_ context.Context, since time.Time, siteID *int64, top int, _ *string) (ReportResponse, error) {
			gotSince, gotSiteID, gotTop = since, siteID, top
			return ReportResponse{Changes: ReportChangeSummary{Total: 9}, Until: "2026-06-09T12:00:00Z"}, nil
		},
	})
	t.Cleanup(ts.Close)
	c := NewClientWithBaseURL(ts.URL, "tok")
	site := int64(3)
	since := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	resp, err := c.Report(context.Background(), since, &site, 5, nil)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.Changes.Total != 9 || resp.Until == "" {
		t.Fatalf("resp = %+v", resp)
	}
	// since must round-trip in UTC RFC3339 (not local time, not a different key).
	if !gotSince.Equal(since) {
		t.Fatalf("server since = %v, want %v", gotSince, since)
	}
	if gotSiteID == nil || *gotSiteID != 3 {
		t.Fatalf("server site_id = %v, want 3", gotSiteID)
	}
	if gotTop != 5 {
		t.Fatalf("server top = %d, want 5", gotTop)
	}
}

// TestClientReadsEncodeSegment proves the client puts segment on the wire for both
// reads (and omits it when empty/nil), so the daemon's ?segment= parse can fire (A7).
func TestClientReadsEncodeSegment(t *testing.T) {
	t.Parallel()
	var gotIssueSeg string
	var gotReportSeg *string
	c := newClientAgainst(t, Hooks{
		Issues: func(_ context.Context, q IssueQuery) ([]IssueView, error) {
			gotIssueSeg = q.Segment
			return nil, nil
		},
		Report: func(_ context.Context, _ time.Time, _ *int64, _ int, segment *string) (ReportResponse, error) {
			gotReportSeg = segment
			return ReportResponse{}, nil
		},
	})

	if _, err := c.Issues(context.Background(), nil, "", "", "content"); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if gotIssueSeg != "content" {
		t.Fatalf("Issues segment over wire = %q, want content", gotIssueSeg)
	}

	seg := "product"
	if _, err := c.Report(context.Background(), time.Time{}, nil, 0, &seg); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotReportSeg == nil || *gotReportSeg != "product" {
		t.Fatalf("Report segment over wire = %v, want product", gotReportSeg)
	}

	// Empty/nil segment is omitted — the hook sees the no-filter shape.
	gotIssueSeg, gotReportSeg = "sentinel", &seg
	if _, err := c.Issues(context.Background(), nil, "", "", ""); err != nil {
		t.Fatalf("Issues(no segment): %v", err)
	}
	if gotIssueSeg != "" {
		t.Fatalf("empty issue segment leaked %q over the wire", gotIssueSeg)
	}
	if _, err := c.Report(context.Background(), time.Time{}, nil, 0, nil); err != nil {
		t.Fatalf("Report(no segment): %v", err)
	}
	if gotReportSeg != nil {
		t.Fatalf("nil report segment leaked %v over the wire", gotReportSeg)
	}
}

// TestClientReport_OmitsZeroParams proves the omit branches fire: a zero since,
// nil siteID, and top<=0 must NOT emit since=/site_id=/top= params. Reaching a
// LIVE handler (not a dead server) is what makes this an assertion — the handler
// only sees zero/nil/0 if the client genuinely left the params off the URL (a
// spurious since=0001-01-01 would fail RFC3339 parse → 400; a spurious top=0
// would surface as a non-empty raw the handler still parses to 0, but site_id=
// would have to be a non-empty string).
func TestClientReport_OmitsZeroParams(t *testing.T) {
	t.Parallel()
	var (
		called    bool
		gotSince  time.Time
		gotSiteID *int64
		gotTop    int
		gotRawQ   string
	)
	srv := NewServer(ServerOptions{Token: "tok", Version: "0.1.0", Hooks: Hooks{
		Report: func(ctx context.Context, since time.Time, siteID *int64, top int, _ *string) (ReportResponse, error) {
			called = true
			gotSince, gotSiteID, gotTop = since, siteID, top
			if r, ok := ctx.Value(reportRawQueryKey{}).(string); ok {
				gotRawQ = r
			}
			return ReportResponse{}, nil
		},
	}})
	// Wrap the handler to capture the raw query string the client sent, so we can
	// assert no since/site_id/top keys appear at all (not merely that they parsed
	// to zero values).
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), reportRawQueryKey{}, r.URL.RawQuery))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	c := NewClientWithBaseURL(ts.URL, "tok")
	if _, err := c.Report(context.Background(), time.Time{}, nil, 0, nil); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !called {
		t.Fatal("Report hook never invoked")
	}
	if !gotSince.IsZero() || gotSiteID != nil || gotTop != 0 {
		t.Fatalf("omit branches failed: since=%v siteID=%v top=%d", gotSince, gotSiteID, gotTop)
	}
	q, _ := url.ParseQuery(gotRawQ)
	if q.Has("since") || q.Has("site_id") || q.Has("top") {
		t.Fatalf("spurious params on omitted-arg Report: raw query = %q", gotRawQ)
	}
}

// reportRawQueryKey carries the raw query string from the wrapping handler to the
// Report hook so the omit-params test can assert no keys were emitted.
type reportRawQueryKey struct{}

func TestClientReadsDaemonDownMapsSentinel(t *testing.T) {
	// A client pointed at a closed server maps the transport error to
	// ErrDaemonNotRunning for every read — assert the sentinel, not just non-nil.
	c := NewClientWithBaseURL("http://127.0.0.1:1", "tok") // nothing listening
	_, err := c.Sites(context.Background())
	if err == nil {
		t.Fatal("Sites against dead daemon: want error")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("Sites against dead daemon: err = %v, want wrapped ErrDaemonNotRunning", err)
	}

	// Report flows through the same c.do() path; assert the spec-mandated
	// report-specific daemon-down mapping explicitly.
	_, rerr := c.Report(context.Background(), time.Time{}, nil, 0, nil)
	if rerr == nil {
		t.Fatal("Report against dead daemon: want error")
	}
	if !errors.Is(rerr, ErrDaemonNotRunning) {
		t.Fatalf("Report against dead daemon: err = %v, want wrapped ErrDaemonNotRunning", rerr)
	}
}
