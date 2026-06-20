package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestSitesAddCallsControlAPI(t *testing.T) {
	var gotBody control.AddSiteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sites" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer testtoken" {
			t.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(control.AddSiteResponse{SiteID: 42})
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "testtoken")
	id, err := runSitesAdd(context.Background(), client, "https://example.com", "Example", "10m", "24h", 100)
	if err != nil {
		t.Fatalf("runSitesAdd() error = %v", err)
	}
	if id != 42 {
		t.Errorf("site id = %d, want 42", id)
	}
	if gotBody.URL != "https://example.com" || gotBody.Name != "Example" {
		t.Errorf("request body mismatch: %+v", gotBody)
	}
	if gotBody.MinInterval != "10m" || gotBody.Speed != 100 {
		t.Errorf("flags not forwarded: %+v", gotBody)
	}
}

func TestSitesRemoveCallsControlAPI(t *testing.T) {
	var method, path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := runSitesRemove(context.Background(), client, "9", true); err != nil {
		t.Fatalf("runSitesRemove() error = %v", err)
	}
	if method != http.MethodDelete || path != "/v1/sites/9" || query != "purge=true" {
		t.Errorf("remove call = %s %s?%s, want DELETE /v1/sites/9?purge=true", method, path, query)
	}
}

func TestSitePagesLine(t *testing.T) {
	tests := []struct {
		name      string
		monitored int
		cap       int
		want      string
	}{
		{"capped", 2000, 2000, "pages: monitoring 2000 of 2000 cap (capped — raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>'; 0 = all)"},
		{"under cap", 12, 2000, "pages: monitoring 12 of 2000 cap"},
		{"unlimited", 5000, 0, "pages: monitoring 5000 (cap: unlimited)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sitePagesLine(tc.monitored, tc.cap); got != tc.want {
				t.Fatalf("sitePagesLine(%d,%d)\n got: %q\nwant: %q", tc.monitored, tc.cap, got, tc.want)
			}
		})
	}
}
