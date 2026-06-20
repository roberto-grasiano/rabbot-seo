package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReadEndpointsNilHookReturns501 asserts every new GET read route exists and
// returns 501 when its hook is nil (mirrors TestNilHookReturns501 for the
// mutation routes).
func TestReadEndpointsNilHookReturns501(t *testing.T) {
	ts := newTestServer(Hooks{}) // no hooks wired
	t.Cleanup(ts.Close)

	tests := []struct {
		name string
		path string
	}{
		{"list sites", "/v1/sites"},
		{"site detail", "/v1/sites/1/detail"},
		{"list issues", "/v1/issues"},
		{"history", "/v1/history?url=https://x.example/"},
		{"report", "/v1/report"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+tc.path, nil)
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotImplemented {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("GET %s status = %d, want 501 (body=%s)", tc.path, resp.StatusCode, body)
			}
		})
	}
}

// TestReadEndpointsRequireToken asserts the new GET routes are behind auth too.
func TestReadEndpointsRequireToken(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)

	for _, path := range []string{"/v1/sites", "/v1/sites/1/detail", "/v1/issues", "/v1/history?url=x", "/v1/report"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		// no Authorization header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, resp.StatusCode)
		}
	}
}

// getJSON is a small helper: GET path with the test token, decode the JSON body
// into out, return the status code.
func getJSON(t *testing.T, ts *httptest.Server, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if derr := json.NewDecoder(resp.Body).Decode(out); derr != nil && resp.StatusCode == http.StatusOK {
			t.Fatalf("decode %s: %v", path, derr)
		}
	}
	return resp.StatusCode
}

func TestListSitesHook(t *testing.T) {
	ts := newTestServer(Hooks{
		ListSites: func(context.Context) ([]SiteSummary, error) {
			return []SiteSummary{
				{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"},
				{ID: 2, URL: "https://b.test", Enabled: false, VerificationState: "throttled"},
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got SitesResponse
	if code := getJSON(t, ts, "/v1/sites", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Sites) != 2 || got.Sites[0].VerificationState != "verified" || got.Sites[1].VerificationState != "throttled" {
		t.Fatalf("unexpected sites payload: %+v", got.Sites)
	}
}

func TestSiteDetailHookFound(t *testing.T) {
	ts := newTestServer(Hooks{
		SiteDetail: func(_ context.Context, id int64) (SiteDetailResponse, bool, error) {
			return SiteDetailResponse{ID: id, URL: "https://a.test", VerificationState: "verified", OpenIssueCount: 3, HasSnapshot: true, Title: "Home"}, true, nil
		},
	})
	t.Cleanup(ts.Close)

	var got SiteDetailResponse
	if code := getJSON(t, ts, "/v1/sites/7/detail", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.ID != 7 || got.OpenIssueCount != 3 || !got.HasSnapshot || got.Title != "Home" {
		t.Fatalf("unexpected detail payload: %+v", got)
	}
}

func TestSiteDetailHookNotFoundIsData(t *testing.T) {
	ts := newTestServer(Hooks{
		SiteDetail: func(context.Context, int64) (SiteDetailResponse, bool, error) {
			return SiteDetailResponse{}, false, nil
		},
	})
	t.Cleanup(ts.Close)

	var nf NotFoundResponse
	// Not-found is HTTP 200 with not_found:true (data, not an error).
	if code := getJSON(t, ts, "/v1/sites/999/detail", &nf); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not-found-as-data)", code)
	}
	if !nf.NotFound || nf.Error == "" {
		t.Fatalf("want structured not-found, got %+v", nf)
	}
}

// TestSiteDetailResponseIndexableFalseSerializes pins that a genuine
// indexable=false is present in the control JSON (no omitempty), so the field
// survives the wire and a consumer can tell "not indexable" from "field absent".
func TestSiteDetailResponseIndexableFalseSerializes(t *testing.T) {
	raw, err := json.Marshal(SiteDetailResponse{ID: 5, HasSnapshot: true, Indexable: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"indexable":false`) {
		t.Fatalf("indexable=false was dropped from JSON: %s", raw)
	}
}

func TestSiteDetailBadID(t *testing.T) {
	ts := newTestServer(Hooks{
		SiteDetail: func(context.Context, int64) (SiteDetailResponse, bool, error) {
			return SiteDetailResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/sites/abc/detail", nil); code != http.StatusBadRequest {
		t.Fatalf("non-numeric id status = %d, want 400", code)
	}
}

func TestListIssuesHookPassesQuery(t *testing.T) {
	var gotQuery IssueQuery
	ts := newTestServer(Hooks{
		Issues: func(_ context.Context, q IssueQuery) ([]IssueView, error) {
			gotQuery = q
			return []IssueView{{ID: 5, RuleID: "missing-title", Severity: "critical", Status: "open"}}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got IssuesResponse
	if code := getJSON(t, ts, "/v1/issues?site_id=42&severity=critical&status=open", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotQuery.SiteID == nil || *gotQuery.SiteID != 42 || gotQuery.Severity != "critical" || gotQuery.Status != "open" {
		t.Fatalf("hook saw query %+v, want site=42 sev=critical status=open", gotQuery)
	}
	if len(got.Issues) != 1 || got.Issues[0].RuleID != "missing-title" {
		t.Fatalf("unexpected issues payload: %+v", got.Issues)
	}
}

// TestListIssuesParsesSegment asserts the handler parses ?segment= onto
// IssueQuery.Segment (A7). An absent param leaves it empty.
func TestListIssuesParsesSegment(t *testing.T) {
	var gotQuery IssueQuery
	ts := newTestServer(Hooks{
		Issues: func(_ context.Context, q IssueQuery) ([]IssueView, error) {
			gotQuery = q
			return nil, nil
		},
	})
	t.Cleanup(ts.Close)

	if code := getJSON(t, ts, "/v1/issues?segment=content", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotQuery.Segment != "content" {
		t.Fatalf("hook saw segment %q, want content", gotQuery.Segment)
	}

	// Absent ?segment= => empty (no filter).
	gotQuery = IssueQuery{}
	if code := getJSON(t, ts, "/v1/issues", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotQuery.Segment != "" {
		t.Fatalf("absent segment parsed as %q, want empty", gotQuery.Segment)
	}
}

// TestHandleReportParsesSegment asserts the report handler parses ?segment= into a
// non-nil *string and passes it to the hook; absent => nil (no filter).
func TestHandleReportParsesSegment(t *testing.T) {
	var gotSegment *string
	ts := newTestServer(Hooks{
		Report: func(_ context.Context, _ time.Time, _ *int64, _ int, segment *string) (ReportResponse, error) {
			gotSegment = segment
			return ReportResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)

	if code := getJSON(t, ts, "/v1/report?segment=content", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotSegment == nil || *gotSegment != "content" {
		t.Fatalf("report hook saw segment %v, want content", gotSegment)
	}

	gotSegment = nil
	if code := getJSON(t, ts, "/v1/report", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotSegment != nil {
		t.Fatalf("absent segment parsed as %v, want nil", gotSegment)
	}
}

// TestSiteDetailResponseSegmentsSerialize pins that the Segments list rides the
// SiteDetailResponse JSON with the documented snake_case fields (name, match,
// member_count) so the mcp-local SegmentView decodes straight in (the seam).
func TestSiteDetailResponseSegmentsSerialize(t *testing.T) {
	raw, err := json.Marshal(SiteDetailResponse{
		ID:       5,
		Segments: []SegmentSummary{{Name: "content", Match: "^/blog/", MemberCount: 3}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"segments":[`, `"name":"content"`, `"match":"^/blog/"`, `"member_count":3`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("SiteDetailResponse JSON missing %q: %s", want, raw)
		}
	}
}

func TestListIssuesBadSiteID(t *testing.T) {
	ts := newTestServer(Hooks{
		Issues: func(context.Context, IssueQuery) ([]IssueView, error) { return nil, nil },
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/issues?site_id=oops", nil); code != http.StatusBadRequest {
		t.Fatalf("bad site_id status = %d, want 400", code)
	}
}

func TestListIssuesBadEnumIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		Issues: func(context.Context, IssueQuery) ([]IssueView, error) {
			return nil, ErrBadRequest // hook rejects an invalid severity enum
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/issues?severity=nonsense", nil); code != http.StatusBadRequest {
		t.Fatalf("invalid severity status = %d, want 400", code)
	}
}

func TestHistoryHookMissingURLIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		History: func(context.Context, string, time.Time) (HistoryResponse, error) {
			return HistoryResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/history", nil); code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", code)
	}
}

func TestHistoryHookParsesSince(t *testing.T) {
	var gotURL string
	var gotSince time.Time
	ts := newTestServer(Hooks{
		History: func(_ context.Context, url string, since time.Time) (HistoryResponse, error) {
			gotURL, gotSince = url, since
			return HistoryResponse{URL: url, Changes: []ChangeView{{Field: "title", ChangeClass: "substantive"}}}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got HistoryResponse
	if code := getJSON(t, ts, "/v1/history?url=https://a.test/&since=2026-01-02T03:04:05Z", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotURL != "https://a.test/" {
		t.Fatalf("hook url = %q, want https://a.test/", gotURL)
	}
	if gotSince.IsZero() || gotSince.UTC().Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
		t.Fatalf("hook since = %v, want parsed 2026-01-02T03:04:05Z", gotSince)
	}
	if len(got.Changes) != 1 || got.Changes[0].Field != "title" {
		t.Fatalf("unexpected history payload: %+v", got.Changes)
	}
}

func TestHistoryHookBadSinceIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		History: func(context.Context, string, time.Time) (HistoryResponse, error) {
			return HistoryResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/history?url=x&since=not-a-time", nil); code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", code)
	}
}

func TestHandleReport_OK(t *testing.T) {
	t.Parallel()
	var gotSince time.Time
	var gotSite *int64
	var gotTop int
	ts := newTestServer(Hooks{
		Report: func(_ context.Context, since time.Time, siteID *int64, top int, _ *string) (ReportResponse, error) {
			gotSince, gotSite, gotTop = since, siteID, top
			return ReportResponse{
				Changes: ReportChangeSummary{Total: 2, Substantive: 1, Cosmetic: 1},
				Issues:  ReportIssueSummary{OpenTotal: 1, OpenCritical: 1},
				TopURLs: []ReportURLChange{{URLID: 5, URL: "https://x/p", Count: 2, LastChanged: "2026-06-09T11:00:00Z"}},
			}, nil
		},
	})
	t.Cleanup(ts.Close)
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	var resp ReportResponse
	code := getJSON(t, ts, "/v1/report?since="+since.Format(time.RFC3339)+"&site_id=7&top=3", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !gotSince.Equal(since) || gotSite == nil || *gotSite != 7 || gotTop != 3 {
		t.Fatalf("hook args: since=%v site=%v top=%d", gotSince, gotSite, gotTop)
	}
	if resp.Changes.Total != 2 || resp.Issues.OpenCritical != 1 || len(resp.TopURLs) != 1 {
		t.Fatalf("decoded resp = %+v", resp)
	}
	// envelope stamped by the handler
	if resp.Since == "" || resp.Until == "" {
		t.Fatalf("Since/Until not stamped: %+v", resp)
	}
	if _, err := time.Parse(time.RFC3339, resp.Until); err != nil {
		t.Fatalf("Until not RFC3339: %q", resp.Until)
	}
}

func TestHandleReport_BadParams(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Report: func(context.Context, time.Time, *int64, int, *string) (ReportResponse, error) {
			return ReportResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/report?since=not-a-time", "/v1/report?since=2026-06-02T12:00:00Z&site_id=abc", "/v1/report?top=-2"} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

func TestHandleReport_NilHook(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/report", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestHandleReport_OmittedSinceStampsZeroTime guards the always-stamp behaviour:
// when ?since= is omitted the wire Since must be a valid RFC3339 zero time
// ("0001-01-01T00:00:00Z"), never an empty string — so a consumer (e.g. the MCP
// ReportView) always sees a timestamp, not an ambiguous "".
func TestHandleReport_OmittedSinceStampsZeroTime(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Report: func(context.Context, time.Time, *int64, int, *string) (ReportResponse, error) {
			return ReportResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	var resp ReportResponse
	if code := getJSON(t, ts, "/v1/report", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Since != "0001-01-01T00:00:00Z" {
		t.Fatalf("omitted since stamped as %q, want %q", resp.Since, "0001-01-01T00:00:00Z")
	}
}
