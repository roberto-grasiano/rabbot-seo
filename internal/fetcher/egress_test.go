package fetcher

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEgressIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7\n"))
	}))
	defer srv.Close()

	// allowPrivate=true: the httptest server is on loopback, which the SSRF guard
	// rejects by default (the guard itself is covered by ssrf tests).
	info, err := EgressIP(context.Background(), srv.URL, "", true)
	if err != nil {
		t.Fatalf("EgressIP() error = %v", err)
	}
	if len(info.IPs) != 1 || info.IPs[0] != "203.0.113.7" {
		t.Errorf("IPs = %v, want [203.0.113.7]", info.IPs)
	}
	if net.ParseIP(info.IPs[0]) == nil {
		t.Errorf("IP %q not parseable", info.IPs[0])
	}
	if info.Endpoint != srv.URL {
		t.Errorf("Endpoint = %q, want %q", info.Endpoint, srv.URL)
	}
	if info.CheckedAt.IsZero() {
		t.Errorf("CheckedAt not set")
	}
}

func TestEgressIPError(t *testing.T) {
	_, err := EgressIP(context.Background(), "http://127.0.0.1:1", "", true)
	if err == nil {
		t.Errorf("EgressIP() expected error for dead endpoint")
	}
}

// TestEgressIPBadProxyFailsClosed (F19) verifies that a non-empty but
// unparseable proxy_url makes EgressIP fail closed (error) rather than silently
// egressing directly from the daemon's real IP — mirroring newClient.
func TestEgressIPBadProxyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7\n"))
	}))
	defer srv.Close()

	_, err := EgressIP(context.Background(), srv.URL, "://missing-scheme", true)
	if err == nil {
		t.Fatalf("EgressIP() with malformed proxy_url should fail closed, got nil error")
	}
	if !errors.Is(err, errBadProxyURL) {
		t.Errorf("err = %v, want errBadProxyURL", err)
	}
}

func TestEgressIPSSRFGuardBlocksLoopback(t *testing.T) {
	// A live loopback server that WOULD answer — but allowPrivate=false must make
	// the SSRF dial guard refuse it (defense against a misconfigured endpoint
	// pointing at an internal/metadata address).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7\n"))
	}))
	defer srv.Close()

	if _, err := EgressIP(context.Background(), srv.URL, "", false); err == nil {
		t.Error("EgressIP() with allowPrivate=false must reject a loopback endpoint")
	}
}
