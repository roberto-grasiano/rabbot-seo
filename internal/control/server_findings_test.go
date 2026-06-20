package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAddSiteMapsBadRequest is the regression for finding #20.3 (control half):
// handleAddSite must translate a hook error that wraps ErrBadRequest into HTTP
// 400, any other hook error into 500, and success into 200. Previously every
// non-nil hook error became a 500, so caller-validation failures (bad URL, bad
// interval) were reported as server faults.
func TestAddSiteMapsBadRequest(t *testing.T) {
	tests := []struct {
		name       string
		hookErr    error
		wantStatus int
	}{
		{
			name:       "wrapped ErrBadRequest -> 400",
			hookErr:    fmt.Errorf("bad url: %w", ErrBadRequest),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "opaque error -> 500",
			hookErr:    errors.New("disk fail"),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "success -> 200",
			hookErr:    nil,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hookCalled := false
			ts := newTestServer(Hooks{
				AddSite: func(ctx context.Context, req AddSiteRequest) (AddSiteResponse, error) {
					hookCalled = true
					if tc.hookErr != nil {
						return AddSiteResponse{}, tc.hookErr
					}
					return AddSiteResponse{SiteID: 7}, nil
				},
			})
			t.Cleanup(ts.Close)

			body := strings.NewReader(`{"url":"https://x.example"}`)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sites", body)
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			payload, _ := io.ReadAll(resp.Body)
			if !hookCalled {
				t.Fatal("AddSite hook was not invoked")
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, payload)
			}
		})
	}
}

// TestOversizedBodyReturns413 is the regression for finding Low-413: a request
// body that exceeds maxControlBody must surface as 413 Request Entity Too Large,
// not the generic 400 the http.MaxBytesReader error previously produced when it
// flowed through json.Decode into the StatusBadRequest branch. The body-decoding
// handlers (handleAddSite/handleCrawl/handleNotifyTest/handleConfigSet/handleVerify)
// must detect *http.MaxBytesError via errors.As and map it to 413.
func TestOversizedBodyReturns413(t *testing.T) {
	ts := newTestServer(Hooks{
		AddSite: func(ctx context.Context, req AddSiteRequest) (AddSiteResponse, error) {
			return AddSiteResponse{SiteID: 1}, nil
		},
		Crawl: func(ctx context.Context, req CrawlRequest) (CrawlResponse, error) {
			return CrawlResponse{}, nil
		},
		NotifyTest: func(ctx context.Context, notifier string) error { return nil },
		SetConfig:  func(ctx context.Context, req ConfigSetRequest) error { return nil },
		Verify: func(ctx context.Context, req VerifyRequest) (VerifyResponse, error) {
			return VerifyResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)

	// A JSON document larger than maxControlBody (1 MiB). Valid JSON so the only
	// possible failure is the MaxBytesReader limit, not a parse error.
	huge := `{"url":"` + strings.Repeat("a", maxControlBody+1024) + `"}`

	endpoints := []string{"/v1/sites", "/v1/crawl", "/v1/notify/test", "/v1/config", "/v1/verify"}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+ep, strings.NewReader(huge))
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want 413 (body=%s)", resp.StatusCode, body)
			}
		})
	}
}

// TestAuthRejectionCases is the regression for finding #17.2 (security): the
// auth gate must 401 a missing header, a wrong scheme/prefix, and a wrong
// token, and must admit only a correct "Bearer <token>". The token comparison
// must remain constant-time — the refactor compares the whole Authorization
// header against the expected "Bearer "+token via subtle.ConstantTimeCompare,
// so there is no variable-time byte comparison on the secret region.
func TestAuthRejectionCases(t *testing.T) {
	ts := newTestServer(Hooks{})
	t.Cleanup(ts.Close)

	tests := []struct {
		name       string
		setHeader  bool
		authHeader string
		wantStatus int
	}{
		{"missing header", false, "", http.StatusUnauthorized},
		{"empty header value", true, "", http.StatusUnauthorized},
		{"wrong scheme prefix", true, "Basic tok", http.StatusUnauthorized},
		{"prefix only, empty token", true, "Bearer ", http.StatusUnauthorized},
		{"no space after scheme", true, "Bearertok", http.StatusUnauthorized},
		{"wrong token", true, "Bearer nope", http.StatusUnauthorized},
		{"correct bearer token", true, "Bearer tok", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/health", nil)
			if tc.setHeader {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}
