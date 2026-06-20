package frontier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRobotsAllowDisallow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\nCrawl-delay: 7\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", 5*time.Minute)

	if allowed := rc.Allowed(context.Background(), srv.URL+"/public/page"); !allowed {
		t.Errorf("/public/page should be allowed")
	}
	if allowed := rc.Allowed(context.Background(), srv.URL+"/private/secret"); allowed {
		t.Errorf("/private/secret should be disallowed")
	}
	if d := rc.CrawlDelay(context.Background(), srv.URL+"/x"); d != 7*time.Second {
		t.Errorf("CrawlDelay = %v, want 7s", d)
	}
}

func TestRobotsMissingAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", 5*time.Minute)
	if allowed := rc.Allowed(context.Background(), srv.URL+"/anything"); !allowed {
		t.Errorf("missing robots.txt should allow all")
	}
}

func TestRobotsCacheReusesUntilTTL(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /x/\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Hour)
	_ = rc.Allowed(context.Background(), srv.URL+"/a")
	_ = rc.Allowed(context.Background(), srv.URL+"/b")
	if hits != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (cached)", hits)
	}
}

// F12: a 5xx robots.txt response must result in a full disallow (RFC-9309 /
// Google spec: 5xx is a temporary error => full-disallow), not fail-open.
func TestRobots5xxDisallows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", 5*time.Minute)
	if rc.Allowed(context.Background(), srv.URL+"/anything") {
		t.Errorf("5xx robots.txt should DISALLOW (full disallow per RFC-9309), got allowed")
	}
}

// F12: an error-derived verdict (5xx) must not be pinned for the full TTL — once
// the origin recovers (200 with real rules) on the next call, the cache must
// recheck rather than serving the stale error verdict for the full 5 minutes.
func TestRobotsErrorVerdictNotPinnedForFullTTL(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	}))
	defer srv.Close()

	// Full TTL of 1h; if error verdicts were cached at full TTL, the recovery
	// below would be ignored for an hour. With a short negative TTL the second
	// call (after the negative window) rechecks.
	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Hour)

	if rc.Allowed(context.Background(), srv.URL+"/p") {
		t.Fatalf("5xx should disallow on first call")
	}

	// Recover the origin and expire the (short) negative TTL. If the error
	// verdict had been cached at the full TTL, expiresAt would be ~1h out and
	// shifting it back 2 minutes would still leave the entry fresh; because the
	// 5xx verdict is cached only for the short negative TTL, the entry is now
	// stale and the next call must re-fetch.
	fail.Store(false)
	rc.mu.Lock()
	for k, e := range rc.hosts {
		e.expiresAt = e.expiresAt.Add(-2 * time.Minute)
		rc.hosts[k] = e
	}
	rc.mu.Unlock()

	if !rc.Allowed(context.Background(), srv.URL+"/p") {
		t.Errorf("after recovery and negative-TTL expiry, robots should be re-fetched and allow; verdict was pinned at full TTL")
	}
}

// F29: concurrent first-hits on a single origin must collapse into ONE
// robots.txt fetch (single-flight), not a thundering herd of N fetches.
func TestRobotsConcurrentFirstHitSingleFlight(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(30 * time.Millisecond) // widen the window for the herd
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /x/\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rc.Allowed(context.Background(), srv.URL+"/a")
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("robots.txt fetched %d times under concurrent first-hits, want 1 (single-flight)", got)
	}
}

// TestRobotsPerHostUserAgent pins that when a per-host UA func is wired
// (SetUserAgentFunc), the robots.txt request carries the host-resolved UA instead
// of the static one. The daemon uses this so a robots.txt fetch sends the same
// per-site trust signal the page/sitemap fetches do.
func TestRobotsPerHostUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			gotUA = r.Header.Get("User-Agent")
		}
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "STATIC-UA", 5*time.Minute)
	rc.SetUserAgentFunc(func(host string) string { return "PERHOST-UA for " + host })

	if _, _, err := rc.Raw(context.Background(), srv.URL); err != nil {
		t.Fatalf("Raw() error = %v", err)
	}
	if gotUA != "PERHOST-UA for 127.0.0.1" {
		t.Errorf("robots.txt UA = %q, want per-host %q", gotUA, "PERHOST-UA for 127.0.0.1")
	}
	if gotUA == "STATIC-UA" {
		t.Errorf("static UA leaked through despite SetUserAgentFunc")
	}
}

func TestRobotsRawFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	}))
	defer srv.Close()

	rc := NewRobotsCache(srv.Client(), "Rabbot-SEO/test", 5*time.Minute)
	raw, status, err := rc.Raw(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Raw() error = %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(raw) != "User-agent: *\nAllow: /\n" {
		t.Errorf("raw = %q", raw)
	}
}
