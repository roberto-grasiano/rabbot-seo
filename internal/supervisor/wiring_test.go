package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

func TestBuildAlertingStack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack-critical", Type: "slack-webhook", URL: "https://hooks.slack.com/services/T/B/X"},
			{Name: "slack-digest", Type: "slack-webhook", URL: "https://hooks.slack.com/services/T/B/Y"},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{"severity": "critical"}, Notifier: "slack-critical"},
			{Match: map[string]string{}, Notifier: "slack-digest"},
		},
		Alerting: config.AlertingConfig{
			DedupWindow: "5m", PerRecipientHourlyCap: 30, IncidentAutoCloseAfter: "24h",
			Digest: config.DigestConfig{Schedule: "1h", Severities: []string{"info", "warning"}},
		},
	}

	stack, err := BuildAlertingStack(cfg, st, http.DefaultClient, func() time.Time { return time.Now() }, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}
	if stack.Processor == nil {
		t.Error("expected a non-nil Processor")
	}
	if stack.Pipeline == nil {
		t.Error("expected a non-nil alerts Pipeline")
	}
	if _, ok := stack.Registry.Get("slack-critical"); !ok {
		t.Error("registry should contain slack-critical")
	}
}

// TestBuildAlertingStackFromOnboardingConfig is the end-to-end contract test for
// the onboarding alerts step. It writes a config exactly the way `rabbot init
// --slack-webhook` does (config.AddNotifierYAML + config.AddRouteYAML), loads it,
// and feeds it through BuildAlertingStack. This proves two things the unit tests
// in internal/cli cannot:
//
//  1. TYPE CONTRACT: the notifier type the onboarding step writes is accepted by
//     BuildAlertingStack's type switch. A regression to type "slack" would make
//     this return `unknown notifier type "slack"` and fail daemon startup.
//  2. ROUTE CONTRACT: a sample alert actually routes to the configured notifier.
//     Without the fallback route the onboarding step now writes, Route returns no
//     notifiers and real change alerts would silently never be dispatched.
func TestBuildAlertingStackFromOnboardingConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")

	// Exactly mirror applyAlertsStep: notifier (slack-webhook) + fallback route.
	if err := config.AddNotifierYAML(cfgPath, config.NotifierConfig{
		Name: "slack", Type: "slack-webhook", URL: "https://hooks.slack.com/services/T/B/X",
	}); err != nil {
		t.Fatalf("AddNotifierYAML: %v", err)
	}
	if err := config.AddRouteYAML(cfgPath, config.RouteConfig{Notifier: "slack"}); err != nil {
		t.Fatalf("AddRouteYAML: %v", err)
	}

	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	stack, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil)
	if err != nil {
		// A regression to notifier type "slack" surfaces here.
		t.Fatalf("BuildAlertingStack rejected the onboarding-written config: %v", err)
	}

	// The fallback route must resolve a real change alert to the slack notifier;
	// without it the notifier is unreachable (no implicit all-notifiers fallback).
	got := stack.Registry.Route(notify.Alert{
		Site:       "https://example.com",
		ChangeType: "title",
		Severity:   model.SeverityWarning,
		DetectedAt: time.Now(),
	})
	if len(got) != 1 {
		t.Fatalf("onboarding config does not route a real change alert to any notifier (got %d); the notifier is unreachable", len(got))
	}
	if got[0].Name() != "slack" {
		t.Fatalf("alert routed to %q, want the onboarding slack notifier", got[0].Name())
	}
}

// TestDigestFlushDeliversOverCapAlert verifies the over-cap path: a non-critical
// alert that exceeds the per-recipient hourly cap is accrued to the internal
// digest buffer (not delivered immediately) and is then delivered when the daemon
// invokes stack.DigestFlush. With HourlyCap=0 the first non-critical event is
// already over cap, so it must route to the buffer; DigestFlush drains it and
// dispatches via the local dispatcher to the routed (slack-webhook) notifier.
func TestDigestFlushDeliversOverCapAlert(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack-digest", Type: "slack-webhook", URL: srv.URL},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{}, Notifier: "slack-digest"}, // catch-all
		},
		Alerting: config.AlertingConfig{
			DedupWindow: "5m", PerRecipientHourlyCap: 1, IncidentAutoCloseAfter: "24h",
		},
	}

	stack, err := BuildAlertingStack(cfg, st, srv.Client(), time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}
	if stack.DigestFlush == nil {
		t.Fatal("expected a non-nil DigestFlush closure")
	}

	ctx := context.Background()
	// A site row must exist: OpenIncident has a FOREIGN KEY on site_id.
	siteID, err := st.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "example", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// With HourlyCap=1, the FIRST non-critical to this recipient (site|severity) is
	// delivered live; a SECOND one (distinct change_type => distinct incident, but
	// the SAME throttle recipient "example.com|warning") is over cap and must accrue
	// to the digest buffer instead of dispatching now.
	first := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, Before: "Old", After: "New",
	}
	if err := stack.Pipeline.Ingest(ctx, first); err != nil {
		t.Fatalf("Ingest first: %v", err)
	}
	second := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/b",
		ChangeType: "meta_description", Severity: model.SeverityWarning, Before: "x", After: "y",
	}
	if err := stack.Pipeline.Ingest(ctx, second); err != nil {
		t.Fatalf("Ingest second: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("first (under cap) delivered live, second (over cap) buffered; want 1 live delivery, got %d", got)
	}

	// Draining the digest buffer must deliver the accrued (second) alert.
	stack.DigestFlush(ctx)
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("DigestFlush should deliver the buffered alert; want 2 total, got %d", got)
	}

	// A second flush must be a no-op (buffer already drained).
	stack.DigestFlush(ctx)
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("second DigestFlush should be a no-op; got %d total deliveries", got)
	}
}

// TestDigestFlushReBuffersOnCtxCancel (F16) verifies that when ctx is already
// cancelled, DigestFlush dispatches nothing and the buffered alerts are NOT lost:
// they remain in the buffer for the next flush. The pre-fix code drained the
// whole buffer up front and silently discarded the remainder on cancel.
func TestDigestFlushReBuffersOnCtxCancel(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack-digest", Type: "slack-webhook", URL: srv.URL},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{}, Notifier: "slack-digest"},
		},
		Alerting: config.AlertingConfig{
			DedupWindow: "5m", PerRecipientHourlyCap: 1, IncidentAutoCloseAfter: "24h",
		},
	}

	stack, err := BuildAlertingStack(cfg, st, srv.Client(), time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	ctx := context.Background()
	siteID, err := st.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "example", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// HourlyCap=1: the first non-critical delivers live; the second (same throttle
	// recipient) is over cap and accrues to the digest buffer.
	first := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, Before: "Old", After: "New",
	}
	if err := stack.Pipeline.Ingest(ctx, first); err != nil {
		t.Fatalf("Ingest first: %v", err)
	}
	second := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/b",
		ChangeType: "meta_description", Severity: model.SeverityWarning, Before: "x", After: "y",
	}
	if err := stack.Pipeline.Ingest(ctx, second); err != nil {
		t.Fatalf("Ingest second: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("first delivers live, second buffers; want 1 live delivery, got %d", got)
	}

	// Flush with an already-cancelled ctx: must dispatch nothing AND keep the alert.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	stack.DigestFlush(cancelled)
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("cancelled flush must dispatch nothing more; got %d", got)
	}

	// A subsequent live flush must still deliver the (re-buffered) alert.
	stack.DigestFlush(ctx)
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("alert must survive a cancelled flush and deliver on the next; want 2, got %d", got)
	}
}

// TestDigestBufferCap (F48) verifies the digest buffer enforces a maximum and
// drops the oldest entries once the cap is exceeded, rather than growing without
// bound.
func TestDigestBufferCap(t *testing.T) {
	b := &digestBuffer{max: 3}
	for i := 0; i < 5; i++ {
		b.Add(notify.Alert{ChangeType: "title", After: string(rune('a' + i))})
	}
	out := b.drain()
	if len(out) != 3 {
		t.Fatalf("buffer should cap at 3, got %d", len(out))
	}
	// Drop-oldest: the last 3 added (c, d, e) survive.
	if out[0].After != "c" || out[2].After != "e" {
		t.Fatalf("expected newest 3 retained [c,d,e], got [%s,%s,%s]", out[0].After, out[1].After, out[2].After)
	}
	if b.dropped != 2 {
		t.Fatalf("expected dropped counter = 2, got %d", b.dropped)
	}
}

// TestBuildAlertingStackLogsBadDuration (F49) verifies a malformed alerting
// duration string produces a warning log instead of silently falling back.
func TestBuildAlertingStackLogsBadDuration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg := config.Config{
		Alerting: config.AlertingConfig{
			DedupWindow: "5min", IncidentAutoCloseAfter: "7day", // both malformed
		},
	}
	if _, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, log, nil); err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "5min") || !strings.Contains(logged, "7day") {
		t.Fatalf("expected a warning naming each bad duration; got:\n%s", logged)
	}
}

func TestBuildAlertingStackRejectsUnknownNotifierType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, _ := store.Open(context.Background(), dbPath)
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Config{
		Notifiers: []config.NotifierConfig{{Name: "x", Type: "carrier-pigeon", URL: "nope"}},
		Alerting:  config.AlertingConfig{DedupWindow: "5m", IncidentAutoCloseAfter: "24h"},
	}
	if _, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil); err == nil {
		t.Error("expected an error for unknown notifier type")
	}
}
