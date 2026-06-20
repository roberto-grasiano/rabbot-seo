package frontier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRobotsCacheSitemaps(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: https://ex.com/sitemap_index.xml\nSitemap: https://ex.com/news.xml\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Minute)
	got := rc.Sitemaps(context.Background(), srv.URL+"/some/page")
	want := map[string]bool{"https://ex.com/sitemap_index.xml": true, "https://ex.com/news.xml": true}
	if len(got) != 2 {
		t.Fatalf("Sitemaps len=%d, want 2: %v", len(got), got)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected sitemap %q", s)
		}
	}
}
