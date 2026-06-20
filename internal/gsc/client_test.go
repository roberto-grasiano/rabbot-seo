package gsc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// staticProvider is a test TokenProvider that hands back a fixed bearer string
// and records how many times it was asked, so tests can assert the client adds
// the Authorization header from the provider on every call.
type staticProvider struct {
	token string
	calls int
}

func (s *staticProvider) Token(_ context.Context) (string, error) {
	s.calls++
	return s.token, nil
}

func (s *staticProvider) Mode() string { return "test" }

// newTestClient wires a Client at the httptest base URL with a static token
// provider, so no live credentials are needed.
func newTestClient(t *testing.T, baseURL, inspectBaseURL, token string) *Client {
	t.Helper()
	c, err := NewClient(Options{
		Token:          &staticProvider{token: token},
		HTTPClient:     srvClient(t),
		BaseURL:        baseURL,
		InspectBaseURL: inspectBaseURL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// srvClient returns an http.Client with a short timeout for talking to httptest.
func srvClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Timeout: 5 * time.Second}
}

func TestSearchAnalyticsQuery_URLPrefixProperty(t *testing.T) {
	const token = "tok-abc"
	var gotPath, gotAuth, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath() // the on-the-wire (escaped) path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"rows": [
				{"keys": ["https://ex.com/a", "shoes"], "clicks": 10, "impressions": 100, "ctr": 0.1, "position": 4.5},
				{"keys": ["https://ex.com/b", "boots"], "clicks": 0, "impressions": 5, "ctr": 0, "position": 22.0}
			],
			"responseAggregationType": "byProperty"
		}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, token)
	resp, err := c.SearchAnalyticsQuery(context.Background(), "https://ex.com/", SearchAnalyticsRequest{
		StartDate:  "2026-06-01",
		EndDate:    "2026-06-10",
		Dimensions: []string{"page", "query"},
		DataState:  "final",
		RowLimit:   1000,
	})
	if err != nil {
		t.Fatalf("SearchAnalyticsQuery: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// URL-prefix property must be percent-encoded in the path segment.
	wantPath := "/webmasters/v3/sites/https%3A%2F%2Fex.com%2F/searchAnalytics/query"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("auth = %q, want Bearer %s", gotAuth, token)
	}
	// dataState=final must be in the body (the partial-data lag discipline).
	if !strings.Contains(gotBody, `"dataState":"final"`) {
		t.Errorf("request body missing dataState=final: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"startDate":"2026-06-01"`) {
		t.Errorf("request body missing startDate: %s", gotBody)
	}

	if len(resp.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.Rows))
	}
	r0 := resp.Rows[0]
	if len(r0.Keys) < 2 || r0.Keys[0] != "https://ex.com/a" || r0.Keys[1] != "shoes" {
		t.Errorf("row0 keys = %v, want [https://ex.com/a shoes]", r0.Keys)
	}
	if r0.Clicks != 10 || r0.Impressions != 100 || r0.CTR != 0.1 || r0.Position != 4.5 {
		t.Errorf("row0 metrics = %+v", r0)
	}
}

func TestSearchAnalyticsQuery_DomainProperty_Escaping(t *testing.T) {
	var gotPath, gotDecoded string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath() // the on-the-wire (escaped) path
		gotDecoded = r.URL.Path       // the decoded path Google's router sees
		_, _ = io.WriteString(w, `{"rows": []}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	_, err := c.SearchAnalyticsQuery(context.Background(), "sc-domain:ex.com", SearchAnalyticsRequest{
		StartDate: "2026-06-01", EndDate: "2026-06-10", DataState: "final",
	})
	if err != nil {
		t.Fatalf("SearchAnalyticsQuery: %v", err)
	}
	// The colon in sc-domain:ex.com must be percent-encoded so it is one path segment.
	wantPath := "/webmasters/v3/sites/sc-domain%3Aex.com/searchAnalytics/query"
	if gotPath != wantPath {
		t.Errorf("domain-property path = %q, want %q", gotPath, wantPath)
	}
	// And it must decode back to exactly the property Google's router expects.
	wantDecoded := "/webmasters/v3/sites/sc-domain:ex.com/searchAnalytics/query"
	if gotDecoded != wantDecoded {
		t.Errorf("domain-property decoded path = %q, want %q", gotDecoded, wantDecoded)
	}
}

func TestInspectURL_ParsesIndexStatus(t *testing.T) {
	const token = "tok-xyz"
	var gotPath, gotBody, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{
			"inspectionResult": {
				"indexStatusResult": {
					"verdict": "PASS",
					"coverageState": "Submitted and indexed",
					"indexingState": "INDEXING_ALLOWED",
					"robotsTxtState": "ALLOWED",
					"pageFetchState": "SUCCESSFUL",
					"lastCrawlTime": "2026-06-15T08:30:00Z",
					"googleCanonical": "https://ex.com/page",
					"userCanonical": "https://ex.com/page",
					"crawledAs": "DESKTOP"
				}
			}
		}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, token)
	res, err := c.InspectURL(context.Background(), "https://ex.com/", "https://ex.com/page")
	if err != nil {
		t.Fatalf("InspectURL: %v", err)
	}

	if gotPath != "/v1/urlInspection/index:inspect" {
		t.Errorf("path = %q, want /v1/urlInspection/index:inspect", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"inspectionUrl":"https://ex.com/page"`) {
		t.Errorf("body missing inspectionUrl: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"siteUrl":"https://ex.com/"`) {
		t.Errorf("body missing siteUrl: %s", gotBody)
	}

	idx := res.InspectionResult.IndexStatusResult
	if idx.Verdict != "PASS" {
		t.Errorf("verdict = %q, want PASS", idx.Verdict)
	}
	if idx.CoverageState != "Submitted and indexed" {
		t.Errorf("coverageState = %q", idx.CoverageState)
	}
	if idx.IndexingState != "INDEXING_ALLOWED" {
		t.Errorf("indexingState = %q", idx.IndexingState)
	}
	if idx.GoogleCanonical != "https://ex.com/page" || idx.UserCanonical != "https://ex.com/page" {
		t.Errorf("canonicals = %q / %q", idx.GoogleCanonical, idx.UserCanonical)
	}
	wantCrawl := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
	if !idx.LastCrawlTime.Equal(wantCrawl) {
		t.Errorf("lastCrawlTime = %v, want %v", idx.LastCrawlTime, wantCrawl)
	}
}

func TestInspectURL_AbsentLastCrawlTimeIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A never-crawled URL omits lastCrawlTime, googleCanonical, etc.
		_, _ = io.WriteString(w, `{
			"inspectionResult": {
				"indexStatusResult": {
					"verdict": "NEUTRAL",
					"coverageState": "URL is unknown to Google"
				}
			}
		}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	res, err := c.InspectURL(context.Background(), "https://ex.com/", "https://ex.com/new")
	if err != nil {
		t.Fatalf("InspectURL: %v", err)
	}
	idx := res.InspectionResult.IndexStatusResult
	if !idx.LastCrawlTime.IsZero() {
		t.Errorf("lastCrawlTime = %v, want zero for an absent field", idx.LastCrawlTime)
	}
	if idx.GoogleCanonical != "" {
		t.Errorf("googleCanonical = %q, want empty", idx.GoogleCanonical)
	}
}

func TestListSites_ParsesEntries(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = io.WriteString(w, `{
			"siteEntry": [
				{"siteUrl": "https://ex.com/", "permissionLevel": "siteFullUser"},
				{"siteUrl": "sc-domain:ex.com", "permissionLevel": "siteOwner"}
			]
		}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	resp, err := c.ListSites(context.Background())
	if err != nil {
		t.Fatalf("ListSites: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/webmasters/v3/sites" {
		t.Errorf("path = %q, want /webmasters/v3/sites", gotPath)
	}
	if len(resp.SiteEntry) != 2 {
		t.Fatalf("siteEntry = %d, want 2", len(resp.SiteEntry))
	}
	if resp.SiteEntry[0].SiteURL != "https://ex.com/" || resp.SiteEntry[0].PermissionLevel != "siteFullUser" {
		t.Errorf("entry0 = %+v", resp.SiteEntry[0])
	}
}

func TestClient_APIErrorIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error": {"code": 403, "message": "User does not have sufficient permission", "status": "PERMISSION_DENIED"}}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	_, err := c.ListSites(context.Background())
	if err == nil {
		t.Fatal("expected an error for 403, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.Code != 403 {
		t.Errorf("APIError.Code = %d, want 403", apiErr.Code)
	}
	if apiErr.Status != "PERMISSION_DENIED" {
		t.Errorf("APIError.Status = %q", apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "sufficient permission") {
		t.Errorf("APIError.Message = %q", apiErr.Message)
	}
}

func TestClient_QuotaExceededIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error": {"code": 429, "message": "Quota exceeded for quota metric 'Queries'", "status": "RESOURCE_EXHAUSTED"}}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, srv.URL, "t")
	_, err := c.InspectURL(context.Background(), "https://ex.com/", "https://ex.com/p")
	if err == nil {
		t.Fatal("expected an error for 429, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T", err)
	}
	if apiErr.Code != 429 {
		t.Errorf("Code = %d, want 429", apiErr.Code)
	}
	if !apiErr.IsQuotaExceeded() {
		t.Errorf("IsQuotaExceeded() = false, want true for 429 RESOURCE_EXHAUSTED")
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable(429) = false, want true")
	}
}

func TestClient_PropertyValidation(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "t")
	for _, prop := range []string{"", "ex.com", "ftp://ex.com/"} {
		if _, err := c.SearchAnalyticsQuery(context.Background(), prop, SearchAnalyticsRequest{
			StartDate: "2026-06-01", EndDate: "2026-06-02",
		}); err == nil {
			t.Errorf("SearchAnalyticsQuery(%q) = nil error, want validation error", prop)
		}
	}
}

func TestClient_TokenProviderErrorPropagates(t *testing.T) {
	c, err := NewClient(Options{
		Token:          &errProvider{},
		HTTPClient:     srvClient(t),
		BaseURL:        "http://127.0.0.1:1",
		InspectBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListSites(context.Background()); err == nil {
		t.Fatal("expected the token-provider error to propagate, got nil")
	}
}

type errProvider struct{}

func (errProvider) Token(_ context.Context) (string, error) {
	return "", errTokenProvider
}
func (errProvider) Mode() string { return "err" }

var errTokenProvider = newSentinel("gsc: token unavailable")

// jsonRoundTrip is a small sanity check that our request body marshals omitempty
// correctly (no empty dataState/dimensions leaking when unset).
func TestSearchAnalyticsRequest_OmitsEmpty(t *testing.T) {
	b, err := json.Marshal(SearchAnalyticsRequest{StartDate: "2026-06-01", EndDate: "2026-06-02"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "dimensions") || strings.Contains(s, "dataState") || strings.Contains(s, "rowLimit") {
		t.Errorf("unset optional fields leaked into JSON: %s", s)
	}
}
