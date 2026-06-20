package control

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestScoreHook_OK asserts GET /v1/score?site_id=N returns 200 + the DTO with a
// valid token, and the handler passes site_id, segment, and since through.
func TestScoreHook_OK(t *testing.T) {
	t.Parallel()
	var (
		gotSite  int64
		gotSeg   string
		gotSince time.Time
	)
	ts := newTestServer(Hooks{
		Score: func(_ context.Context, siteID int64, segment string, since time.Time) (ScoreResponse, bool, error) {
			gotSite, gotSeg, gotSince = siteID, segment, since
			return ScoreResponse{
				SiteID:        siteID,
				Defined:       true,
				Score:         87.5,
				ImpactMass:    125,
				MaxMass:       1000,
				KnownURLs:     10,
				ProcessedURLs: 8,
				OpenCritical:  1,
				PageCount:     8,
				Breakdown:     `{"title-missing":125}`,
				Series: []ScorePoint{
					{ComputedAt: "2026-06-10T00:00:00Z", Score: 100, MaxMass: 1000, PageCount: 8},
					{ComputedAt: "2026-06-11T00:00:00Z", Score: 87.5, ImpactMass: 125, MaxMass: 1000, PageCount: 8},
				},
			}, true, nil
		},
	})
	t.Cleanup(ts.Close)

	var resp ScoreResponse
	if code := getJSON(t, ts, "/v1/score?site_id=7&segment=content&since=2026-06-09T00:00:00Z", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotSite != 7 || gotSeg != "content" || !gotSince.Equal(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("hook args = site %d seg %q since %v", gotSite, gotSeg, gotSince)
	}
	if !resp.Defined || resp.Score != 87.5 || resp.MaxMass != 1000 || resp.KnownURLs != 10 ||
		resp.ProcessedURLs != 8 || len(resp.Series) != 2 {
		t.Fatalf("unexpected score payload: %+v", resp)
	}
}

// TestScoreHook_MissingSiteID asserts a missing/blank/non-numeric site_id is a 400.
func TestScoreHook_MissingSiteID(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Score: func(context.Context, int64, string, time.Time) (ScoreResponse, bool, error) {
			return ScoreResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/score", "/v1/score?site_id=abc"} {
		if code := getJSON(t, ts, path, nil); code != http.StatusBadRequest {
			t.Fatalf("path %q: status = %d, want 400", path, code)
		}
	}
}

// TestScoreHook_MalformedSince asserts a malformed since (not RFC3339) is a 400.
func TestScoreHook_MalformedSince(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Score: func(context.Context, int64, string, time.Time) (ScoreResponse, bool, error) {
			return ScoreResponse{}, true, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/score?site_id=1&since=not-a-time", nil); code != http.StatusBadRequest {
		t.Fatalf("malformed since status = %d, want 400", code)
	}
}

// TestScoreHook_NotFound asserts an unknown site/segment is surfaced as data (HTTP
// 200 NotFoundResponse), matching handleSiteDetail — NOT a 404.
func TestScoreHook_NotFound(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{
		Score: func(context.Context, int64, string, time.Time) (ScoreResponse, bool, error) {
			return ScoreResponse{}, false, nil
		},
	})
	t.Cleanup(ts.Close)
	var raw struct {
		NotFound bool `json:"not_found"`
	}
	if code := getJSON(t, ts, "/v1/score?site_id=999", &raw); code != http.StatusOK {
		t.Fatalf("unknown site status = %d, want 200 (errors-as-data)", code)
	}
	if !raw.NotFound {
		t.Fatalf("unknown site should set not_found=true; got %+v", raw)
	}
}

// TestScoreHook_NilHook asserts the route returns 501 when unwired (the hooks.Report pattern).
func TestScoreHook_NilHook(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/score?site_id=1", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestScoreRequiresToken asserts the route is behind auth (401 without token).
func TestScoreRequiresToken(t *testing.T) {
	t.Parallel()
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/score?site_id=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}
}

// TestClientScore asserts Client.Score round-trips the DTO with found=true.
func TestClientScore(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Score: func(_ context.Context, siteID int64, _ string, _ time.Time) (ScoreResponse, bool, error) {
			return ScoreResponse{SiteID: siteID, Defined: true, Score: 90, MaxMass: 1000, KnownURLs: 5, ProcessedURLs: 5}, true, nil
		},
	})
	got, found, err := c.Score(context.Background(), 9, "", time.Time{})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !found || !got.Defined || got.Score != 90 || got.SiteID != 9 {
		t.Fatalf("Score = %+v found=%v", got, found)
	}
}

// TestClientScoreNotFound asserts an unknown site/segment is surfaced as found=false
// with a nil error (errors-as-data for the MCP bridge), matching SiteDetailFound.
func TestClientScoreNotFound(t *testing.T) {
	t.Parallel()
	c := newClientAgainst(t, Hooks{
		Score: func(context.Context, int64, string, time.Time) (ScoreResponse, bool, error) {
			return ScoreResponse{}, false, nil
		},
	})
	_, found, err := c.Score(context.Background(), 999, "", time.Time{})
	if err != nil {
		t.Fatalf("Score unknown err = %v, want nil", err)
	}
	if found {
		t.Fatalf("Score unknown found = true, want false")
	}
}
