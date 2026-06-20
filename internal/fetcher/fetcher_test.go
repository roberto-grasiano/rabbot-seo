package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func newTestFetcher() Fetcher {
	return New(Options{
		UserAgent:    "Rabbot-SEO/test (+https://example.test)",
		Timeout:      5 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true, // tests target httptest servers on 127.0.0.1
	})
}

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "Rabbot-SEO/test (+https://example.test)" {
			t.Errorf("missing UA, got %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2025 07:28:00 GMT")
		w.Header().Set("X-Robots-Tag", "noindex")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html><title>Hello</title></html>"))
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", res.HTTPStatus)
	}
	if res.FetchClass != model.FetchOK {
		t.Errorf("FetchClass = %q, want ok", res.FetchClass)
	}
	if res.StatusType != model.StatusPage {
		t.Errorf("StatusType = %q, want page", res.StatusType)
	}
	if !strings.Contains(string(res.Body), "Hello") {
		t.Errorf("Body missing content: %q", res.Body)
	}
	if res.Header.Get("ETag") != `"abc"` {
		t.Errorf("ETag header lost")
	}
	if res.Header.Get("X-Robots-Tag") != "noindex" {
		t.Errorf("X-Robots-Tag header lost")
	}
	if res.ResponseTime <= 0 {
		t.Errorf("ResponseTime not measured")
	}
}

// TestFetchUserAgentFuncOverridesStatic pins that when Options.UserAgentFunc is
// set, the per-request User-Agent header is computed from the target host via the
// func (not the static Options.UserAgent), and that a nil func keeps the static
// back-compat behavior. The func is keyed on httpReq.URL.Hostname(), so two
// distinct loopback ports (127.0.0.1 in both) still resolve through it.
func TestFetchUserAgentFuncOverridesStatic(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := New(Options{
		UserAgent:    "STATIC-UA",
		Timeout:      5 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
		UserAgentFunc: func(host string) string {
			return "PERHOST-UA for " + host
		},
	})
	if _, err := f.Fetch(context.Background(), Request{URL: srv.URL}); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if gotUA != "PERHOST-UA for 127.0.0.1" {
		t.Errorf("UserAgentFunc not applied: got UA %q, want %q", gotUA, "PERHOST-UA for 127.0.0.1")
	}
	if gotUA == "STATIC-UA" {
		t.Errorf("static UA leaked through despite UserAgentFunc being set")
	}

	// A nil func keeps the static UA (back-compat).
	gotUA = ""
	f2 := New(Options{
		UserAgent:    "STATIC-UA",
		Timeout:      5 * time.Second,
		MaxBodyBytes: 1 << 20,
		AllowPrivate: true,
	})
	if _, err := f2.Fetch(context.Background(), Request{URL: srv.URL}); err != nil {
		t.Fatalf("Fetch() (nil func) error = %v", err)
	}
	if gotUA != "STATIC-UA" {
		t.Errorf("nil UserAgentFunc should keep the static UA: got %q", gotUA)
	}
}

func TestFetchRedirectChain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusMovedPermanently) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("done")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL + "/a"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(res.RedirectChain) != 3 {
		t.Fatalf("RedirectChain len = %d, want 3: %v", len(res.RedirectChain), res.RedirectChain)
	}
	if !strings.HasSuffix(res.RedirectChain[0], "/a") || !strings.HasSuffix(res.FinalURL, "/c") {
		t.Errorf("chain = %v, finalURL = %q", res.RedirectChain, res.FinalURL)
	}
	if res.StatusType != model.StatusRedirect {
		t.Errorf("StatusType = %q, want redirect", res.StatusType)
	}
}

func TestFetchConditionalGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(304)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL, ETag: `"abc"`})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !res.NotModified {
		t.Errorf("NotModified = false, want true")
	}
	if res.HTTPStatus != 304 {
		t.Errorf("HTTPStatus = %d, want 304", res.HTTPStatus)
	}
	if res.Body != nil {
		t.Errorf("Body should be nil on 304, got %q", res.Body)
	}
}

func TestFetchSoftBlockNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FetchClass != model.FetchSoftBlock {
		t.Errorf("FetchClass = %q, want soft_block", res.FetchClass)
	}
	if res.Body != nil {
		t.Errorf("Body should be nil for non-ok class, got %q", res.Body)
	}
	if res.Header.Get("Retry-After") != "120" {
		t.Errorf("Retry-After header lost")
	}
}

func TestFetchHardBlockDetector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html><title>Attention Required! | Cloudflare</title></html>"))
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FetchClass != model.FetchHardBlock {
		t.Errorf("FetchClass = %q, want hard_block", res.FetchClass)
	}
	if res.Detector != "cloudflare" {
		t.Errorf("Detector = %q, want cloudflare", res.Detector)
	}
	if res.Body != nil {
		t.Errorf("Body should be nil for non-ok class, got %q", res.Body)
	}
}

func TestFetchUnreachable(t *testing.T) {
	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Fetch() should not return error for unreachable, got %v", err)
	}
	if res.FetchClass != model.FetchUnreachable {
		t.Errorf("FetchClass = %q, want unreachable", res.FetchClass)
	}
	if res.Err == nil {
		t.Errorf("Result.Err should be set on unreachable")
	}
	if res.StatusType != model.StatusUnreachable {
		t.Errorf("StatusType = %q, want unreachable", res.StatusType)
	}
}

func TestFetchAccessSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Rabbot-Token") != "secret" {
			t.Errorf("custom header lost")
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "user" || p != "pass" {
			t.Errorf("basic auth lost: %q %q %v", u, p, ok)
		}
		c, err := r.Cookie("sess")
		if err != nil || c.Value != "xyz" {
			t.Errorf("cookie lost: %v %v", c, err)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req := Request{
		URL:       srv.URL,
		Headers:   map[string]string{"X-Rabbot-Token": "secret"},
		BasicUser: "user",
		BasicPass: "pass",
		Cookies:   []*http.Cookie{{Name: "sess", Value: "xyz"}},
	}
	res, err := newTestFetcher().Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FetchClass != model.FetchOK {
		t.Errorf("FetchClass = %q, want ok", res.FetchClass)
	}
}
