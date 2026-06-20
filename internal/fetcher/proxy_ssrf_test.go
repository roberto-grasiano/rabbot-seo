package fetcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestNewClientProxySSRFFailsClosed (F20.4) verifies that, when the SSRF guard is
// active (AllowPrivate=false), a proxy_url whose host is a disallowed IP literal
// (cloud metadata / private range) makes newClient fail closed at build time with
// a clear error — rather than handing back a client that would attempt to egress
// to the proxy and only fail late at dial time. This is defense-in-depth: the
// dialer.Control hook also rejects the proxy dial, but a config-time error is
// clearer and survives any future transport rewiring.
func TestNewClientProxySSRFFailsClosed(t *testing.T) {
	disallowed := []string{
		"http://169.254.169.254",      // cloud instance metadata
		"http://169.254.169.254:3128", // metadata with port
		"http://10.0.0.1:3128",        // RFC1918 private
		"http://127.0.0.1:8080",       // loopback
		"http://[::1]:8080",           // IPv6 loopback
		"https://192.168.1.1:3128",    // private, https proxy scheme
	}
	for _, p := range disallowed {
		c, err := newClient(Options{Timeout: time.Second}, p)
		if err == nil {
			t.Errorf("newClient(AllowPrivate=false, proxyURL=%q) = nil error, want SSRF rejection", p)
		}
		if !errors.Is(err, errDisallowedDestination) {
			t.Errorf("newClient(proxyURL=%q) err = %v, want errDisallowedDestination", p, err)
		}
		if c != nil {
			t.Errorf("newClient(proxyURL=%q) returned a non-nil client on rejection; must refuse to build an egressing client", p)
		}
	}
}

// TestNewClientProxyAllowPrivatePermitsInternal verifies the guard respects
// AllowPrivate: when set (test wiring targeting loopback), an internal proxy host
// is permitted, mirroring the dialer which skips Control under AllowPrivate.
func TestNewClientProxyAllowPrivatePermitsInternal(t *testing.T) {
	for _, p := range []string{
		"http://169.254.169.254",
		"http://10.0.0.1:3128",
		"http://127.0.0.1:8080",
	} {
		c, err := newClient(Options{Timeout: time.Second, AllowPrivate: true}, p)
		if err != nil {
			t.Errorf("newClient(AllowPrivate=true, proxyURL=%q) = %v, want nil (private proxy permitted under AllowPrivate)", p, err)
		}
		if c == nil {
			t.Errorf("newClient(AllowPrivate=true, proxyURL=%q) returned nil client, want a usable client", p)
		}
	}
}

// TestNewClientProxyPublicHostPermitted verifies a normal public proxy host (IP
// literal or name) is still accepted under the active guard — the new check must
// not over-block legitimate proxies.
func TestNewClientProxyPublicHostPermitted(t *testing.T) {
	for _, p := range []string{
		"http://8.8.8.8:3128",         // public IP literal
		"http://proxy.example.com:80", // name-based (validated at dial time)
		"https://proxy.example.com",   // name-based, https scheme
	} {
		c, err := newClient(Options{Timeout: time.Second}, p)
		if err != nil {
			t.Errorf("newClient(proxyURL=%q) = %v, want nil for a public proxy host", p, err)
		}
		if c == nil {
			t.Errorf("newClient(proxyURL=%q) returned nil client, want a usable client", p)
		}
	}
}

// TestNewClientBadProxyStillFailsClosed guards against a regression of the
// existing errBadProxyURL behavior: an unparseable proxy_url must still fail
// closed (and must not be reclassified as the SSRF error).
func TestNewClientBadProxyStillFailsClosed(t *testing.T) {
	c, err := newClient(Options{Timeout: time.Second}, "://missing-scheme")
	if err == nil {
		t.Fatalf("newClient with malformed proxy_url should error, got nil")
	}
	if !errors.Is(err, errBadProxyURL) {
		t.Errorf("err = %v, want errBadProxyURL for an unparseable proxy_url", err)
	}
	if c != nil {
		t.Errorf("newClient returned non-nil client for an unparseable proxy_url")
	}
}

// TestFetchProxySSRFFailsClosed verifies the guard surfaces through Fetch: a
// per-site proxy_url pointing at an internal/metadata host yields FetchUnreachable
// with Result.Err set, never direct egress from the daemon's real IP.
func TestFetchProxySSRFFailsClosed(t *testing.T) {
	f := New(Options{UserAgent: "t", Timeout: time.Second}) // AllowPrivate defaults false
	res, err := f.Fetch(context.Background(), Request{URL: "https://example.com", ProxyURL: "http://169.254.169.254"})
	if err != nil {
		t.Fatalf("Fetch() returned transport error = %v", err)
	}
	if res.FetchClass != model.FetchUnreachable {
		t.Errorf("FetchClass = %q, want unreachable (proxy SSRF must fail closed)", res.FetchClass)
	}
	if res.Err == nil {
		t.Errorf("Result.Err should be set when the proxy host is SSRF-rejected")
	}
	if len(res.Body) != 0 {
		t.Errorf("Body must be empty when the proxy is rejected, got %q", res.Body)
	}
}
