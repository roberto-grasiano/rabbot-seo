package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestCrawlCallsControlAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/crawl":
			var req control.CrawlRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Target != "https://example.com/p" {
				t.Errorf("crawl target = %q", req.Target)
			}
			_ = json.NewEncoder(w).Encode(control.CrawlResponse{Queued: 1})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "tok")
	queued, err := runCrawl(context.Background(), client, "https://example.com/p")
	if err != nil {
		t.Fatalf("runCrawl() error = %v", err)
	}
	if queued != 1 {
		t.Errorf("queued = %d, want 1", queued)
	}
}

func TestPauseResumeCallControlAPI(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(control.OKResponse{OK: true})
	}))
	defer srv.Close()

	client := control.NewClientWithBaseURL(srv.URL, "tok")
	if err := runPause(context.Background(), client); err != nil {
		t.Fatalf("runPause() error = %v", err)
	}
	if err := runResume(context.Background(), client); err != nil {
		t.Fatalf("runResume() error = %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v1/pause" || paths[1] != "/v1/resume" {
		t.Errorf("paths = %v, want [/v1/pause /v1/resume]", paths)
	}
}
