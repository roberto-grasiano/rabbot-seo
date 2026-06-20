package control

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// ─── GET /v1/index-status?url= — GSC index status for a URL (GSC W2) ──────────

// TestIndexStatusHookMissingURLIs400 mirrors handleRichResults: a missing ?url=
// is a caller fault -> 400 (not a not-found).
func TestIndexStatusHookMissingURLIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		IndexStatus: func(context.Context, string) (IndexStatusResponse, error) {
			return IndexStatusResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/index-status", nil); code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", code)
	}
}

// TestIndexStatusHookNilIs501 asserts the route returns 501 when unwired.
func TestIndexStatusHookNilIs501(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/index-status?url=https://x.example/", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestIndexStatusHookRequiresToken asserts the route is behind auth.
func TestIndexStatusHookRequiresToken(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/index-status?url=x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}

// TestIndexStatusHookOK asserts the URL passes through and the DTO round-trips,
// including the verdict/coverage/canonical fields and the optional last_crawl_time.
func TestIndexStatusHookOK(t *testing.T) {
	var gotURL string
	ts := newTestServer(Hooks{
		IndexStatus: func(_ context.Context, url string) (IndexStatusResponse, error) {
			gotURL = url
			return IndexStatusResponse{
				URL:             url,
				HasStatus:       true,
				Verdict:         "PASS",
				CoverageState:   "Submitted and indexed",
				IndexingState:   "INDEXING_ALLOWED",
				RobotsTxtState:  "ALLOWED",
				PageFetchState:  "SUCCESSFUL",
				GoogleCanonical: "https://a.test/p",
				UserCanonical:   "https://a.test/p",
				CrawledAs:       "DESKTOP",
				InspectedAt:     "2026-06-18T00:00:00Z",
				LastCrawlTime:   "2026-06-17T00:00:00Z",
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got IndexStatusResponse
	if code := getJSON(t, ts, "/v1/index-status?url=https://a.test/p", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotURL != "https://a.test/p" {
		t.Fatalf("hook url = %q, want https://a.test/p", gotURL)
	}
	if !got.HasStatus || got.Verdict != "PASS" || got.CoverageState != "Submitted and indexed" ||
		got.GoogleCanonical != "https://a.test/p" || got.LastCrawlTime != "2026-06-17T00:00:00Z" {
		t.Fatalf("decoded resp = %+v", got)
	}
}

// TestIndexStatusHookNotFoundIsData asserts an un-inspected URL is reported as
// data (200 + not_found:true / has_status:false), the handleRichResults pattern —
// NEVER a 404 and NEVER a discrepancy. This is the quota-bounded-staleness guard.
func TestIndexStatusHookNotFoundIsData(t *testing.T) {
	ts := newTestServer(Hooks{
		IndexStatus: func(_ context.Context, url string) (IndexStatusResponse, error) {
			return IndexStatusResponse{URL: url, NotFound: true}, nil
		},
	})
	t.Cleanup(ts.Close)
	var got IndexStatusResponse
	if code := getJSON(t, ts, "/v1/index-status?url=https://a.test/missing", &got); code != http.StatusOK {
		t.Fatalf("not-found status = %d, want 200 (data, not 404)", code)
	}
	if !got.NotFound || got.HasStatus {
		t.Fatalf("want not_found=true has_status=false, got %+v", got)
	}
}

// ─── GET /v1/search-performance?url=&since= — GSC search metrics (GSC W2) ─────

// TestSearchPerformanceHookMissingURLIs400 asserts a missing ?url= is a 400.
func TestSearchPerformanceHookMissingURLIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		SearchPerformance: func(context.Context, string, string) (SearchPerformanceResponse, error) {
			return SearchPerformanceResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/search-performance", nil); code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", code)
	}
}

// TestSearchPerformanceHookBadSinceIs400 asserts a malformed since (not RFC3339)
// is a caller fault -> 400 (the handleScore since-parse contract).
func TestSearchPerformanceHookBadSinceIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		SearchPerformance: func(context.Context, string, string) (SearchPerformanceResponse, error) {
			return SearchPerformanceResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/search-performance?url=https://a.test/p&since=yesterday", nil); code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", code)
	}
}

// TestSearchPerformanceHookNilIs501 asserts the route returns 501 when unwired.
func TestSearchPerformanceHookNilIs501(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/search-performance?url=https://x.example/", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestSearchPerformanceHookOK asserts the URL + since pass through and the rows
// round-trip; the handler stamps the since/until envelope.
func TestSearchPerformanceHookOK(t *testing.T) {
	var gotURL, gotSince string
	ts := newTestServer(Hooks{
		SearchPerformance: func(_ context.Context, url, since string) (SearchPerformanceResponse, error) {
			gotURL = url
			gotSince = since
			return SearchPerformanceResponse{
				URL: url,
				Rows: []SearchMetricView{
					{Query: "rabbit seo", Date: "2026-06-15", Clicks: 10, Impressions: 100, CTR: 0.1, Position: 4.2},
					{Query: "rabbit seo", Date: "2026-06-14", Clicks: 8, Impressions: 90, CTR: 0.089, Position: 4.6},
				},
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got SearchPerformanceResponse
	if code := getJSON(t, ts, "/v1/search-performance?url=https://a.test/p&since=2026-06-10T00:00:00Z", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotURL != "https://a.test/p" {
		t.Fatalf("hook url = %q", gotURL)
	}
	if gotSince == "" {
		t.Fatalf("hook since not passed through")
	}
	if len(got.Rows) != 2 || got.Rows[0].Query != "rabbit seo" || got.Rows[0].Impressions != 100 {
		t.Fatalf("decoded rows = %+v", got.Rows)
	}
}

// TestSearchPerformanceHookNoDataIsEmpty asserts a URL with no metrics is reported
// as data (200 + empty rows / has_data:false), never a 404 or error — the same
// quota-bounded honesty as index-status.
func TestSearchPerformanceHookNoDataIsEmpty(t *testing.T) {
	ts := newTestServer(Hooks{
		SearchPerformance: func(_ context.Context, url, _ string) (SearchPerformanceResponse, error) {
			return SearchPerformanceResponse{URL: url}, nil
		},
	})
	t.Cleanup(ts.Close)
	var got SearchPerformanceResponse
	if code := getJSON(t, ts, "/v1/search-performance?url=https://a.test/p", &got); code != http.StatusOK {
		t.Fatalf("no-data status = %d, want 200", code)
	}
	if got.HasData {
		t.Fatalf("want has_data=false for no rows, got %+v", got)
	}
}

// TestIndexStatusHookErrorIs500 asserts a hook ERROR (a real store failure, not
// absent data) surfaces as a 500 — distinct from the not-found-as-data path.
func TestIndexStatusHookErrorIs500(t *testing.T) {
	ts := newTestServer(Hooks{
		IndexStatus: func(context.Context, string) (IndexStatusResponse, error) {
			return IndexStatusResponse{}, context.DeadlineExceeded
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/index-status?url=https://a.test/p", nil); code != http.StatusInternalServerError {
		t.Fatalf("hook error status = %d, want 500", code)
	}
}

// TestSearchPerformanceHookErrorIs500 asserts a search-performance hook error is a 500.
func TestSearchPerformanceHookErrorIs500(t *testing.T) {
	ts := newTestServer(Hooks{
		SearchPerformance: func(context.Context, string, string) (SearchPerformanceResponse, error) {
			return SearchPerformanceResponse{}, context.DeadlineExceeded
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/search-performance?url=https://a.test/p", nil); code != http.StatusInternalServerError {
		t.Fatalf("hook error status = %d, want 500", code)
	}
}

// TestSearchPerformanceHookStampsUntilAndEchoesSince asserts the handler stamps a
// fresh Until envelope and echoes the resolved since verbatim (the clock-free hook
// contract). Uses a valid RFC3339 since and asserts the response envelope.
func TestSearchPerformanceHookStampsUntilAndEchoesSince(t *testing.T) {
	const since = "2026-06-10T00:00:00Z"
	ts := newTestServer(Hooks{
		SearchPerformance: func(_ context.Context, url, _ string) (SearchPerformanceResponse, error) {
			// The hook itself leaves Since/Until empty — the handler stamps them.
			return SearchPerformanceResponse{URL: url, HasData: true,
				Rows: []SearchMetricView{{Query: "q", Date: "2026-06-15", Impressions: 5}}}, nil
		},
	})
	t.Cleanup(ts.Close)
	var got SearchPerformanceResponse
	if code := getJSON(t, ts, "/v1/search-performance?url=https://a.test/p&since="+since, &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Since != since {
		t.Fatalf("Since = %q, want the echoed %q", got.Since, since)
	}
	if got.Until == "" {
		t.Fatalf("handler must stamp a non-empty Until envelope")
	}
	if _, err := time.Parse(time.RFC3339, got.Until); err != nil {
		t.Fatalf("Until %q is not RFC3339: %v", got.Until, err)
	}
}

// ─── client round-trips ──────────────────────────────────────────────────────

func TestClientIndexStatus(t *testing.T) {
	c := newClientAgainst(t, Hooks{
		IndexStatus: func(_ context.Context, url string) (IndexStatusResponse, error) {
			return IndexStatusResponse{URL: url, HasStatus: true, Verdict: "PASS", CoverageState: "Submitted and indexed"}, nil
		},
	})
	got, err := c.IndexStatus(context.Background(), "https://a.test/p")
	if err != nil {
		t.Fatalf("IndexStatus: %v", err)
	}
	if !got.HasStatus || got.Verdict != "PASS" {
		t.Fatalf("IndexStatus = %+v", got)
	}
}

func TestClientIndexStatusNotFoundIsData(t *testing.T) {
	c := newClientAgainst(t, Hooks{
		IndexStatus: func(_ context.Context, url string) (IndexStatusResponse, error) {
			return IndexStatusResponse{URL: url, NotFound: true}, nil
		},
	})
	got, err := c.IndexStatus(context.Background(), "https://a.test/missing")
	if err != nil {
		t.Fatalf("IndexStatus not-found must be data, got err: %v", err)
	}
	if !got.NotFound {
		t.Fatalf("want NotFound=true, got %+v", got)
	}
}

// TestClientIndexStatusDaemonDown asserts the client surfaces a transport error
// (the do() error arm) when the daemon is unreachable — distinct from not-found-data.
func TestClientIndexStatusDaemonDown(t *testing.T) {
	c := NewClientWithBaseURL("http://127.0.0.1:1", "tok") // nothing listening
	if _, err := c.IndexStatus(context.Background(), "https://a.test/p"); err == nil {
		t.Fatal("IndexStatus against a dead daemon: want error, got nil")
	}
}

// TestClientSearchPerformanceDaemonDown asserts the search-performance client surfaces
// a transport error (the do() error arm) on an unreachable daemon.
func TestClientSearchPerformanceDaemonDown(t *testing.T) {
	c := NewClientWithBaseURL("http://127.0.0.1:1", "tok")
	if _, err := c.SearchPerformance(context.Background(), "https://a.test/p", "2026-06-10T00:00:00Z"); err == nil {
		t.Fatal("SearchPerformance against a dead daemon: want error, got nil")
	}
}

func TestClientSearchPerformance(t *testing.T) {
	var gotSince string
	c := newClientAgainst(t, Hooks{
		SearchPerformance: func(_ context.Context, url, since string) (SearchPerformanceResponse, error) {
			gotSince = since
			return SearchPerformanceResponse{
				URL:     url,
				HasData: true,
				Rows:    []SearchMetricView{{Query: "q", Date: "2026-06-15", Clicks: 1, Impressions: 2}},
			}, nil
		},
	})
	since := "2026-06-10T00:00:00Z"
	got, err := c.SearchPerformance(context.Background(), "https://a.test/p", since)
	if err != nil {
		t.Fatalf("SearchPerformance: %v", err)
	}
	if gotSince != since {
		t.Fatalf("since not forwarded: got %q want %q", gotSince, since)
	}
	if len(got.Rows) != 1 || got.Rows[0].Query != "q" {
		t.Fatalf("SearchPerformance rows = %+v", got.Rows)
	}
}
