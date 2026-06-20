package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestPrintStatus(t *testing.T) {
	resp := control.StatusResponse{
		Version:     "1.2.3",
		Uptime:      "3h4m",
		Paused:      true,
		SiteCount:   2,
		URLCount:    140,
		DueCount:    5,
		QueueDepth:  3,
		LastCrawlAt: "2026-06-01T11:59:00Z",
		EgressIP:    []string{"203.0.113.7"},
	}
	var buf bytes.Buffer
	if err := printStatus(&buf, resp); err != nil {
		t.Fatalf("printStatus() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"1.2.3", "PAUSED", "140", "due=5", "queue=3", "203.0.113.7"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q\n%s", want, out)
		}
	}
}

func TestPrintStatusShowsCappedSites(t *testing.T) {
	resp := control.StatusResponse{Version: "1", Uptime: "1s", SiteCount: 3, CappedSites: 2}
	var buf bytes.Buffer
	if err := printStatus(&buf, resp); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Capped sites: 2") {
		t.Fatalf("missing capped-sites line:\n%s", out)
	}
	if !strings.Contains(out, "max_pages_per_site") {
		t.Fatalf("capped line must name the knob to raise the cap:\n%s", out)
	}
}

// Criterion 11 (status-half): when the metrics listener is configured, `rabbot
// status` renders its address so an operator (or the Claude-path agent) can see
// where /metrics is served. An empty MetricsAddr (off) renders no metrics line.
func TestPrintStatusShowsMetricsAddr(t *testing.T) {
	resp := control.StatusResponse{Version: "1", Uptime: "1s", MetricsAddr: "127.0.0.1:9464"}
	var buf bytes.Buffer
	if err := printStatus(&buf, resp); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "127.0.0.1:9464") {
		t.Fatalf("status output missing metrics address:\n%s", out)
	}
	if !strings.Contains(out, "Metrics") {
		t.Fatalf("status output missing a Metrics label:\n%s", out)
	}
}

func TestPrintStatusHidesMetricsWhenOff(t *testing.T) {
	resp := control.StatusResponse{Version: "1", Uptime: "1s", MetricsAddr: ""}
	var buf bytes.Buffer
	if err := printStatus(&buf, resp); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	if strings.Contains(buf.String(), "Metrics") {
		t.Fatalf("metrics line shown when MetricsAddr is empty:\n%s", buf.String())
	}
}

func TestPrintStatusHidesCappedWhenZero(t *testing.T) {
	resp := control.StatusResponse{Version: "1", Uptime: "1s", SiteCount: 3, CappedSites: 0}
	var buf bytes.Buffer
	if err := printStatus(&buf, resp); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	if strings.Contains(buf.String(), "Capped sites") {
		t.Fatalf("capped line shown when CappedSites=0:\n%s", buf.String())
	}
}

// withShortStatusRetry shrinks the startup-race retry knobs so the tests poll a
// condition fast instead of sleeping the production interval. It restores them on
// cleanup.
func withShortStatusRetry(t *testing.T) {
	t.Helper()
	prevBudget, prevInterval := statusStartupBudget, statusRetryInterval
	statusStartupBudget = 2 * time.Second
	statusRetryInterval = time.Millisecond
	t.Cleanup(func() {
		statusStartupBudget = prevBudget
		statusRetryInterval = prevInterval
	})
}

// TestStatusRetriesThroughStartupRace pins #87(3): right after `service start` the
// control port is not yet bound (~5-8s), so a status issued immediately gets
// ErrDaemonNotRunning. The command must RETRY briefly and ultimately render the
// status once the daemon answers — not print "daemon not running" on the first
// connection-refused. Falsifiable: the pre-fix code returned the first error.
func TestStatusRetriesThroughStartupRace(t *testing.T) {
	withShortStatusRetry(t)

	const need = 3 // fail this many times (the bind window) before succeeding
	calls := 0
	prev := statusFetchFn
	statusFetchFn = func(_ context.Context, _ *control.Client) (control.StatusResponse, error) {
		calls++
		if calls <= need {
			return control.StatusResponse{}, control.ErrDaemonNotRunning
		}
		return control.StatusResponse{Version: "9.9.9", Uptime: "1s"}, nil
	}
	t.Cleanup(func() { statusFetchFn = prev })

	var errOut bytes.Buffer
	resp, err := fetchStatusWithStartupRetry(context.Background(),
		control.NewClient(7777, "tok"), &errOut)
	if err != nil {
		t.Fatalf("status should have succeeded after the bind window, got: %v", err)
	}
	if resp.Version != "9.9.9" {
		t.Fatalf("got %+v, want the eventual successful status", resp)
	}
	if calls <= need {
		t.Fatalf("expected more than %d fetch attempts (it must retry), got %d", need, calls)
	}
	// The operator is told it is starting (not down) on the first retry.
	if !strings.Contains(errOut.String(), "waiting for daemon") {
		t.Fatalf("expected a starting notice on stderr, got: %q", errOut.String())
	}
}

// TestStatusGivesUpWhenGenuinelyDown pins the other arm: a daemon that never comes
// up must NOT retry forever — the command gives up within the bounded budget and
// surfaces the genuine not-running error.
func TestStatusGivesUpWhenGenuinelyDown(t *testing.T) {
	withShortStatusRetry(t)

	prev := statusFetchFn
	statusFetchFn = func(_ context.Context, _ *control.Client) (control.StatusResponse, error) {
		return control.StatusResponse{}, control.ErrDaemonNotRunning
	}
	t.Cleanup(func() { statusFetchFn = prev })

	start := time.Now()
	_, err := fetchStatusWithStartupRetry(context.Background(),
		control.NewClient(7777, "tok"), &bytes.Buffer{})
	if !errors.Is(err, control.ErrDaemonNotRunning) {
		t.Fatalf("want ErrDaemonNotRunning after the budget, got: %v", err)
	}
	// Bounded: it must not block far past the (shrunk) budget.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retry budget not bounded: elapsed %v", elapsed)
	}
}

// TestStatusDoesNotRetryNonStartupError pins that only the connection-refused
// startup race is retried: any other error (e.g. an auth failure) is returned
// immediately, so a genuine misconfiguration is not masked by a 12s wait.
func TestStatusDoesNotRetryNonStartupError(t *testing.T) {
	withShortStatusRetry(t)

	authErr := fmt.Errorf("status: %w", control.ErrUnauthorized)
	calls := 0
	prev := statusFetchFn
	statusFetchFn = func(_ context.Context, _ *control.Client) (control.StatusResponse, error) {
		calls++
		return control.StatusResponse{}, authErr
	}
	t.Cleanup(func() { statusFetchFn = prev })

	_, err := fetchStatusWithStartupRetry(context.Background(),
		control.NewClient(7777, "tok"), &bytes.Buffer{})
	if !errors.Is(err, control.ErrUnauthorized) {
		t.Fatalf("want the auth error surfaced immediately, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a non-startup error must not be retried; calls = %d, want 1", calls)
	}
}

// TestStatusRetryHonorsContextCancellation pins prompt cancellation: if the context
// is cancelled mid-retry, the loop returns the context error rather than spinning
// to the budget.
func TestStatusRetryHonorsContextCancellation(t *testing.T) {
	prevBudget, prevInterval := statusStartupBudget, statusRetryInterval
	statusStartupBudget = time.Hour // long, so only ctx can end the loop
	statusRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		statusStartupBudget = prevBudget
		statusRetryInterval = prevInterval
	})

	prev := statusFetchFn
	statusFetchFn = func(_ context.Context, _ *control.Client) (control.StatusResponse, error) {
		return control.StatusResponse{}, control.ErrDaemonNotRunning
	}
	t.Cleanup(func() { statusFetchFn = prev })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := fetchStatusWithStartupRetry(ctx, control.NewClient(7777, "tok"), &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got: %v", err)
	}
}
