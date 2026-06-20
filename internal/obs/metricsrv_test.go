package obs_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
)

// startMetricsSrv binds the metrics server on a free loopback port and returns
// its base URL and a stop func. It fails the test on a bind error.
func startMetricsSrv(t *testing.T, m *obs.Metrics) (string, func()) {
	t.Helper()
	srv := obs.NewMetricsServer(m)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-errCh
	}
	return "http://" + ln.Addr().String(), stop
}

// Criterion 5 (handler half): GET /metrics => 200 + rabbot_build_info; POST
// /metrics => 405; any other path => 404.
func TestMetricsServer_GETMetrics200(t *testing.T) {
	m := obs.NewMetrics("v4.5.6")
	base, stop := startMetricsSrv(t, m)
	defer stop()

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rabbot_build_info") {
		t.Fatalf("GET /metrics body missing rabbot_build_info:\n%s", body)
	}
	// The exposition must carry the registered version label, not leak anything else.
	if !strings.Contains(string(body), `version="v4.5.6"`) {
		t.Fatalf("GET /metrics body missing version label:\n%s", body)
	}
}

func TestMetricsServer_POSTMetrics405(t *testing.T) {
	m := obs.NewMetrics("v0")
	base, stop := startMetricsSrv(t, m)
	defer stop()

	resp, err := http.Post(base+"/metrics", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status = %d, want 405", resp.StatusCode)
	}
}

func TestMetricsServer_OtherPath404(t *testing.T) {
	m := obs.NewMetrics("v0")
	base, stop := startMetricsSrv(t, m)
	defer stop()

	for _, path := range []string{"/", "/v1/status", "/metricsx", "/healthz"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// ListenAndServe binds 127.0.0.1 and serves until Shutdown; a bind failure on an
// occupied addr is returned to the caller (the fatal-startup-error contract).
func TestMetricsServer_ListenAndServeBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	srv := obs.NewMetricsServer(obs.NewMetrics("v0"))
	if serveErr := srv.ListenAndServe(addr); serveErr == nil {
		t.Fatal("ListenAndServe on an occupied addr returned nil; want a bind error")
	}
}

func TestMetricsServer_ListenAndServeServesThenStops(t *testing.T) {
	srv := obs.NewMetricsServer(obs.NewMetrics("v0"))
	// Bind a free port via the server itself: ask the OS by using :0, then read
	// the bound addr is not exposed, so bind a listener first to learn a port,
	// release it, and let the server take it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(addr) }()

	// Poll until the server answers (bind is synchronous, but the goroutine may
	// not have entered Serve yet).
	deadline := time.Now().Add(2 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		resp, gerr := http.Get("http://" + addr + "/metrics")
		if gerr == nil {
			_ = resp.Body.Close()
			ok = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("metrics server never answered on the bound addr")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if serr := srv.Shutdown(ctx); serr != nil {
		t.Fatalf("Shutdown: %v", serr)
	}
	if serveErr := <-errCh; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe returned %v, want nil or ErrServerClosed", serveErr)
	}
}
