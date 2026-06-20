package control

import (
	"context"
	"net/http"
	"testing"
)

// TestRichResultsHookMissingURLIs400 mirrors the handleHistory contract: a
// missing ?url= is a caller fault -> 400 (not a not-found).
func TestRichResultsHookMissingURLIs400(t *testing.T) {
	ts := newTestServer(Hooks{
		RichResults: func(context.Context, string) (RichResultsResponse, error) {
			return RichResultsResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/rich-results", nil); code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", code)
	}
}

// TestRichResultsHookNilIs501 asserts the route returns 501 when unwired.
func TestRichResultsHookNilIs501(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	if code := getJSON(t, ts, "/v1/rich-results?url=https://x.example/", nil); code != http.StatusNotImplemented {
		t.Fatalf("nil hook status = %d, want 501", code)
	}
}

// TestRichResultsHookRequiresToken asserts the route is behind auth.
func TestRichResultsHookRequiresToken(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/rich-results?url=x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}

// TestRichResultsHookOK asserts the URL passes through and the DTO round-trips,
// including per-entity eligibility and the profile version.
func TestRichResultsHookOK(t *testing.T) {
	var gotURL string
	ts := newTestServer(Hooks{
		RichResults: func(_ context.Context, url string) (RichResultsResponse, error) {
			gotURL = url
			return RichResultsResponse{
				URL:         url,
				HasSnapshot: true,
				Profile:     "grr-2026.06",
				Entities: []RichResultEntity{
					{Type: "Product", RawType: "Product", Eligible: false, Missing: []string{"name"}, MissingAnyOf: [][]string{{"offers", "review"}}},
				},
				Unprofiled: 1,
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	var got RichResultsResponse
	if code := getJSON(t, ts, "/v1/rich-results?url=https://a.test/p", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if gotURL != "https://a.test/p" {
		t.Fatalf("hook url = %q, want https://a.test/p", gotURL)
	}
	if got.Profile != "grr-2026.06" || !got.HasSnapshot || got.Unprofiled != 1 {
		t.Fatalf("decoded resp = %+v", got)
	}
	if len(got.Entities) != 1 || got.Entities[0].Type != "Product" ||
		got.Entities[0].Eligible || len(got.Entities[0].Missing) != 1 ||
		len(got.Entities[0].MissingAnyOf) != 1 {
		t.Fatalf("decoded entities = %+v", got.Entities)
	}
}

// TestRichResultsHookNotFoundIsData asserts an unknown URL is reported as data
// (200 + not_found:true), following the handleHistory not-found pattern — NOT a
// 404 (the chosen single pattern; see fast-follow-backlog not-found note).
func TestRichResultsHookNotFoundIsData(t *testing.T) {
	ts := newTestServer(Hooks{
		RichResults: func(_ context.Context, url string) (RichResultsResponse, error) {
			return RichResultsResponse{URL: url, NotFound: true}, nil
		},
	})
	t.Cleanup(ts.Close)
	var got RichResultsResponse
	if code := getJSON(t, ts, "/v1/rich-results?url=https://a.test/missing", &got); code != http.StatusOK {
		t.Fatalf("not-found status = %d, want 200 (data, not 404)", code)
	}
	if !got.NotFound {
		t.Fatalf("want not_found=true, got %+v", got)
	}
}
