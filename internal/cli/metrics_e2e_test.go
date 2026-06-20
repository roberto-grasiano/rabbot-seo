package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// scrapeMetrics GETs the metrics listener and returns the body. The caller knows
// the bound addr because it pre-binds a port (then releases it) and hands it to
// the daemon as metrics.addr — mirroring the control-server e2e pattern, since the
// daemon does not expose the OS-chosen port for a :0 bind.
func scrapeMetrics(t *testing.T, addr string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Criterion 5 (daemon e2e, on): with metrics.addr set, a scrape succeeds while the
// daemon runs and the listener stops on shutdown.
func TestRunDaemonMetricsScrapeThenStops(t *testing.T) {
	// Pre-bind a free port to learn it, then release it for the daemon to take.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	metricsAddr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         "",
			DataDir:            t.TempDir(),
			ControlToken:       "tok",
			ControlPort:        0, // control listener off; only the metrics listener under test
			MetricsAddr:        metricsAddr,
			Version:            "1.2.3",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	// Poll until the metrics listener answers.
	deadline := time.Now().Add(3 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		code, body = scrapeMetrics(t, metricsAddr)
		if code == http.StatusOK {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if code != http.StatusOK {
		cancel()
		<-done
		t.Fatalf("metrics scrape never returned 200 (got %d)", code)
	}
	if !strings.Contains(body, "rabbot_build_info") {
		t.Errorf("scrape body missing rabbot_build_info:\n%s", body)
	}
	if !strings.Contains(body, `version="1.2.3"`) {
		t.Errorf("scrape body missing the build version label:\n%s", body)
	}

	cancel()
	select {
	case derr := <-done:
		if derr != nil {
			t.Fatalf("runDaemon returned error on shutdown: %v", derr)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runDaemon did not exit within 6s of shutdown")
	}

	// The listener stopped on shutdown: a fresh scrape now fails to connect.
	if code, _ := scrapeMetrics(t, metricsAddr); code == http.StatusOK {
		t.Fatal("metrics listener still answering after daemon shutdown")
	}
}

// fetchesTotalRe extracts the summed value across all rabbot_fetches_total{class=...}
// series in a scrape body. Each class is its own line (e.g.
// `rabbot_fetches_total{class="ok"} 3`); we sum the trailing values so the test does
// not depend on which FetchClass the loopback origin produced.
var fetchesTotalRe = regexp.MustCompile(`(?m)^rabbot_fetches_total\{[^}]*\}\s+([0-9.]+)\s*$`)

func sumFetchesTotal(body string) float64 {
	var sum float64
	for _, m := range fetchesTotalRe.FindAllStringSubmatch(body, -1) {
		// Values are small non-negative integers in this test; parse leniently.
		var v float64
		for _, c := range m[1] {
			if c == '.' {
				break // integer part is enough for a >0 assertion
			}
			v = v*10 + float64(c-'0')
		}
		sum += v
	}
	return sum
}

// TestRunDaemonMetricsFetchesWired is the regression guard for the critical wiring
// defect: the production daemon (runDaemon) constructs *obs.Metrics but must wire it
// into the crawl choke point (the &scheduler.Crawler{Metrics: ...} field) and the
// alerting stack (supervisor.WithStackMetrics). Before the fix, 5 of 9 rabbot_*
// families were permanently zero on a real daemon because run.go never threaded the
// metrics layer through those seams — and the existing e2e (scrape-then-stops) only
// asserted rabbot_build_info, which comes straight off the registry regardless of
// choke-point wiring, so the half-dead feature sailed past `make test`.
//
// This test drives one real fetch through runDaemon's WIRED pipeline: it seeds a
// config.yaml with a site pointing at a loopback httptest origin (AllowPrivate admits
// 127.0.0.1, exactly as the discovery/concurrency e2e tests do), runs the daemon with
// a metrics listener, and asserts the scrape body reports rabbot_fetches_total > 0 —
// which is reachable ONLY if run.go set Crawler.Metrics. This makes the run.go wiring
// impossible to silently regress. The scrape path itself touches no DB (counters are
// atomics), honoring the no-hot-path-DB lesson.
func TestRunDaemonMetricsFetchesWired(t *testing.T) {
	// Loopback origin: a real indexable page so the crawl path runs end to end and
	// ObserveFetch fires at the choke point.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Home</title>` +
			`<meta name="description" content="welcome to the homepage here">` +
			`</head><body><h1>Hello</h1><p>home page content words here today now</p></body></html>`))
	}))
	defer srv.Close()

	// Pre-bind a free port to learn it, then release it for the daemon's metrics listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	metricsAddr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Seed a config with ONE site at the loopback origin. Single-quoted YAML so any
	// Windows backslash in dir stays literal. Disable discovery so the only fetch is
	// the seed page itself (keeps the crawl bounded and the assertion deterministic).
	seed := "data_dir: '" + dir + "'\n" +
		"crawler:\n  contact_email: 'ops@example.com'\n" +
		"defaults:\n  min_interval: '1s'\n  max_interval: '1m'\n" +
		"  discovery:\n    sitemap: false\n    follow_links: false\n" +
		"sites:\n  - url: '" + srv.URL + "'\n"
	if werr := os.WriteFile(cfgPath, []byte(seed), 0o600); werr != nil {
		t.Fatalf("seed config.yaml: %v", werr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         cfgPath,
			DataDir:            dir,
			ControlToken:       "tok",
			ControlPort:        0, // control listener off; only the metrics listener under test
			MetricsAddr:        metricsAddr,
			Version:            "1.2.3",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
			AllowPrivate:       true, // admit the loopback httptest origin
		})
	}()

	// Poll the scrape until rabbot_fetches_total climbs above zero — i.e. the wired
	// crawl choke point recorded at least one fetch through the running daemon.
	deadline := time.Now().Add(8 * time.Second)
	var lastBody string
	got := false
	for time.Now().Before(deadline) {
		code, body := scrapeMetrics(t, metricsAddr)
		if code == http.StatusOK {
			lastBody = body
			if sumFetchesTotal(body) > 0 {
				got = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case derr := <-done:
		if derr != nil {
			t.Fatalf("runDaemon returned error on shutdown: %v", derr)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runDaemon did not exit within 6s of shutdown")
	}

	if !got {
		t.Fatalf("rabbot_fetches_total never exceeded 0 — the daemon never wired *Metrics into the crawl choke point.\nlast scrape body:\n%s", lastBody)
	}
}

// Criterion 5 (daemon e2e, occupied): a configured-but-unbindable metrics addr is
// a fatal startup error (the F18 pattern), not a silent headless run.
func TestRunDaemonMetricsBindFailureIsFatal(t *testing.T) {
	// Hold the port so the metrics listener's net.Listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	occupied := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         "",
			DataDir:            t.TempDir(),
			ControlToken:       "tok",
			ControlPort:        0,
			MetricsAddr:        occupied, // already bound => metrics listener cannot bind
			Version:            "0.0.1",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	select {
	case derr := <-done:
		if derr == nil {
			t.Fatal("runDaemon returned nil on a metrics-listener bind failure; expected a fatal error")
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("runDaemon did not return within 3s on a metrics bind failure (running headless?)")
	}
}

// Criterion 5 (daemon e2e, off): metrics.addr "" opens no listener — the daemon
// runs normally and nothing answers on a metrics port.
func TestRunDaemonMetricsOffNoListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         "",
			DataDir:            t.TempDir(),
			ControlToken:       "tok",
			ControlPort:        0,
			MetricsAddr:        "", // off
			Version:            "0.0.1",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case derr := <-done:
		if derr != nil {
			t.Fatalf("runDaemon (metrics off) returned error: %v", derr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runDaemon (metrics off) did not exit within 3s")
	}
}
