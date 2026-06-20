package fetcher

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestFetchGzipDecoded (F3) verifies that a gzip-encoded response body is
// transparently decoded by the transport. The fetcher must NOT set a manual
// Accept-Encoding: gzip header — doing so suppresses net/http's automatic
// decompression and leaves the raw compressed bytes in resp.Body, which then
// flow into extraction/simhash as garbage.
func TestFetchGzipDecoded(t *testing.T) {
	const html = "<html><head><title>Gzip Title</title></head><body>real decoded content here</body></html>"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write([]byte(html)); err != nil {
			t.Errorf("gzip write: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Errorf("gzip close: %v", err)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	if !strings.Contains(string(res.Body), "Gzip Title") {
		t.Errorf("Body not decoded; got %q", res.Body)
	}
	if !strings.Contains(string(res.Body), "real decoded content here") {
		t.Errorf("Body not decoded; got %q", res.Body)
	}
	// Transparent decompression strips Content-Encoding from the surfaced header
	// (stdlib behavior). Body must not begin with the gzip magic bytes.
	if len(res.Body) >= 2 && res.Body[0] == 0x1f && res.Body[1] == 0x8b {
		t.Errorf("Body still gzip-compressed (magic 1f 8b): %v", res.Body[:2])
	}
}

// TestFetchRedirectCapExhaustion (F26) verifies that exhausting the redirect cap
// is reported as FetchUnreachable, not a successful FetchOK whose truncated 3xx
// body gets extracted/snapshotted as page content.
func TestFetchRedirectCapExhaustion(t *testing.T) {
	// Infinite redirect loop: every path 302s onward, never terminating, so the
	// chain must hit maxRedirects.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), Request{URL: srv.URL + "/start"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FetchClass != model.FetchUnreachable {
		t.Errorf("FetchClass = %q, want unreachable for redirect-cap exhaustion", res.FetchClass)
	}
	if res.StatusType != model.StatusUnreachable {
		t.Errorf("StatusType = %q, want unreachable", res.StatusType)
	}
	if res.Err == nil {
		t.Errorf("Result.Err should be set when the redirect cap is exhausted")
	}
	if len(res.Body) != 0 {
		t.Errorf("Body must not be retained on redirect-cap exhaustion, got %q", res.Body)
	}
}

// TestFetchBodyTruncation (F27) verifies that a body larger than MaxBodyBytes is
// flagged truncated rather than silently passed downstream as if complete.
func TestFetchBodyTruncation(t *testing.T) {
	const cap = 1024
	// Write well over the cap so LimitReader trims it.
	big := strings.Repeat("a", cap*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	f := New(Options{
		UserAgent:    "t",
		Timeout:      5 * time.Second,
		MaxBodyBytes: cap,
		AllowPrivate: true,
	})
	res, err := f.Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true for an oversized body")
	}
	if int64(len(res.Body)) > cap {
		t.Errorf("retained body len = %d, want <= cap %d", len(res.Body), cap)
	}
}

// TestResponseHeaderTimeoutSet (F33) verifies all three outbound clients set a
// ResponseHeaderTimeout so a slow-header origin cannot pin a worker for the full
// 30s overall timeout (matching the robots client's explicit 10s protection).
func TestResponseHeaderTimeoutSet(t *testing.T) {
	// newClient (page-crawl transport).
	c, err := newClient(Options{Timeout: 30 * time.Second}, "")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("newClient transport type = %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("newClient ResponseHeaderTimeout = %v, want > 0", tr.ResponseHeaderTimeout)
	}

	// GuardedClient (robots cache / external callers).
	gc := GuardedClient(30 * time.Second)
	gtr, ok := gc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("GuardedClient transport type = %T", gc.Transport)
	}
	if gtr.ResponseHeaderTimeout <= 0 {
		t.Errorf("GuardedClient ResponseHeaderTimeout = %v, want > 0", gtr.ResponseHeaderTimeout)
	}
}

// TestFetchBodyNotTruncated verifies the Truncated flag is false for a body that
// fits within the cap (no false positive at the boundary).
func TestFetchBodyNotTruncated(t *testing.T) {
	const cap = 1024
	small := strings.Repeat("a", cap-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(small))
	}))
	defer srv.Close()

	f := New(Options{
		UserAgent:    "t",
		Timeout:      5 * time.Second,
		MaxBodyBytes: cap,
		AllowPrivate: true,
	})
	res, err := f.Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false for an under-cap body")
	}
}
