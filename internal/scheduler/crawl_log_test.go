package scheduler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestCrawlOneLogsHeartbeat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("User-agent: *\nAllow: /\n")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>T</title></head><body><p>words here now</p></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	siteID, err := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "S", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 2, SpeedScale: 100})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	if _, err := st.UpsertURL(ctx, model.URL{SiteID: siteID, URL: srv.URL, FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 1}); err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	var buf bytes.Buffer
	crawler := &Crawler{
		Store:     st,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4}),
		Now:       func() time.Time { return now },
		Logger:    obs.NewLogger(&buf, "info"),
	}
	u, err := st.GetURL(ctx, siteID, srv.URL)
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	if r := crawler.CrawlOne(ctx, u, 600, 86400, ""); r.Err != nil {
		t.Fatalf("CrawlOne: %v", r.Err)
	}
	out := buf.String()
	if !strings.Contains(out, "crawled") || !strings.Contains(out, srv.URL) {
		t.Errorf("expected a 'crawled' heartbeat line mentioning the URL; got:\n%s", out)
	}
}
