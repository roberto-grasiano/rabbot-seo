package control

import (
	"context"
	"net/http"
	"testing"
)

// TestCoverageHook_OK asserts GET /v1/coverage?site_id=N returns 200 + the DTO
// with a valid token (acceptance 9).
func TestCoverageHook_OK(t *testing.T) {
	t.Parallel()
	var gotSite int64
	ts := newTestServer(Hooks{
		Coverage: func(_ context.Context, siteID int64) (CoverageResponse, bool, error) {
			gotSite = siteID
			return CoverageResponse{
				HasSitemap:           true,
				SeedStatus:           200,
				SitemappedUncrawled:  3,
				SitemappedUnadmitted: 1,
				CrawledNotInSitemap:  2,
				SampleUncrawled:      []string{"https://a.test/x"},
				SampleNotInSitemap:   []string{"https://a.test/y"},
			}, true, nil
		},
	})
	t.Cleanup(ts.Close)

	var resp CoverageResponse
	if code := getJSON(t, ts, "/v1/coverage?site_id=7", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotSite != 7 {
		t.Fatalf("hook site id = %d, want 7", gotSite)
	}
	if !resp.HasSitemap || resp.SeedStatus != 200 || resp.SitemappedUncrawled != 3 ||
		resp.SitemappedUnadmitted != 1 || resp.CrawledNotInSitemap != 2 ||
		len(resp.SampleUncrawled) != 1 || len(resp.SampleNotInSitemap) != 1 {
		t.Fatalf("unexpected coverage payload: %+v", resp)
	}
}

// TestCoverageHook_MissingSiteID asserts a missing/blank site_id is a 400.
func TestCoverageHook_MissingSiteID(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Coverage: func(context.Context, int64) (CoverageResponse, bool, error) {
			return CoverageResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/coverage", "/v1/coverage?site_id=abc"} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

// TestCoverageHook_NotFound asserts an unknown site id yields 404 (same semantics
// as handleSiteDetail's not-found, per the A2 surfaces spec).
func TestCoverageHook_NotFound(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Coverage: func(context.Context, int64) (CoverageResponse, bool, error) {
			return CoverageResponse{}, false, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/coverage?site_id=999", nil); code != http.StatusNotFound {
		t.Fatalf("unknown site status = %d, want 404", code)
	}
}

// TestCoverageHook_NilHook asserts the route returns 501 when unwired.
func TestCoverageHook_NilHook(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/coverage?site_id=1", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestCoverageRequiresToken asserts the route is behind auth (401 without token).
func TestCoverageRequiresToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/coverage?site_id=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}
}

// TestClientCoverage asserts Client.Coverage round-trips the DTO with found=true.
func TestClientCoverage(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Coverage: func(_ context.Context, siteID int64) (CoverageResponse, bool, error) {
			return CoverageResponse{
				HasSitemap: true, SeedStatus: 200,
				SitemappedUncrawled: 4, CrawledNotInSitemap: 1,
				SampleUncrawled: []string{"https://a.test/x"},
			}, true, nil
		},
	})
	got, found, err := c.Coverage(context.Background(), 9)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if !found || !got.HasSitemap || got.SeedStatus != 200 || got.SitemappedUncrawled != 4 ||
		got.CrawledNotInSitemap != 1 || len(got.SampleUncrawled) != 1 {
		t.Fatalf("Coverage = %+v found=%v", got, found)
	}
}

// TestClientCoverageNotFound asserts an unknown site (404) is surfaced as
// found=false with a nil error (errors-as-data for the MCP bridge), mirroring
// SiteDetailFound's two-shape contract.
func TestClientCoverageNotFound(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Coverage: func(context.Context, int64) (CoverageResponse, bool, error) {
			return CoverageResponse{}, false, nil
		},
	})
	_, found, err := c.Coverage(context.Background(), 999)
	if err != nil {
		t.Fatalf("Coverage unknown-site err = %v, want nil", err)
	}
	if found {
		t.Fatalf("Coverage unknown-site found = true, want false")
	}
}
