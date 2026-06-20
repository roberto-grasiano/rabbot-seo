package supervisor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/notify"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// openTestStore opens a throwaway store for a wiring test.
func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestBuildAlertingStackBuildsNewTypes (acceptance #8) proves the construction
// switch gains case "email-smtp" and case "generic-webhook": both build a notifier
// that lands in the registry under its configured name.
func TestBuildAlertingStackBuildsNewTypes(t *testing.T) {
	st := openTestStore(t)
	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{
				Name: "ops-mail", Type: "email-smtp",
				SMTPHost: "smtp.example.com", SMTPPort: 587,
				Username: "alerts@example.com", Password: "smtp-secret",
				From: "rabbot@example.com", To: []string{"team@example.com"},
			},
			{
				Name: "glue", Type: "generic-webhook",
				URL:     "https://glue.example/hook",
				Headers: map[string]string{"Authorization": "Bearer glue-secret"},
			},
		},
		Alerting: config.AlertingConfig{DedupWindow: "5m", IncidentAutoCloseAfter: "24h"},
	}

	stack, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}
	mail, ok := stack.Registry.Get("ops-mail")
	if !ok {
		t.Fatal("registry should contain the email-smtp notifier ops-mail")
	}
	if mail.Name() != "ops-mail" {
		t.Errorf("email notifier Name() = %q, want ops-mail", mail.Name())
	}
	glue, ok := stack.Registry.Get("glue")
	if !ok {
		t.Fatal("registry should contain the generic-webhook notifier glue")
	}
	if glue.Name() != "glue" {
		t.Errorf("webhook notifier Name() = %q, want glue", glue.Name())
	}
}

// TestBuildAlertingStackRejectsIncompleteConfig (acceptance #8) proves an
// incomplete email/webhook config fails AT STARTUP (construction), the error names
// the offending notifier, and it NEVER echoes a secret value.
func TestBuildAlertingStackRejectsIncompleteConfig(t *testing.T) {
	const secret = "STARTUP-SECRET-VALUE"
	tests := []struct {
		name     string
		notifier config.NotifierConfig
		wantName string
	}{
		{
			name: "email missing smtp_host/from/to",
			notifier: config.NotifierConfig{
				Name: "bad-mail", Type: "email-smtp", Password: secret,
			},
			wantName: "bad-mail",
		},
		{
			name: "webhook missing url",
			notifier: config.NotifierConfig{
				Name: "bad-glue", Type: "generic-webhook",
				Headers: map[string]string{"Authorization": secret},
			},
			wantName: "bad-glue",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			cfg := config.Config{
				Notifiers: []config.NotifierConfig{tc.notifier},
				Alerting:  config.AlertingConfig{DedupWindow: "5m", IncidentAutoCloseAfter: "24h"},
			}
			_, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil)
			if err == nil {
				t.Fatalf("expected a startup error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("startup error should name the notifier %q; got %q", tc.wantName, err.Error())
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("startup error leaked a secret value: %q", err.Error())
			}
		})
	}
}

// TestRouteAndThrottleKeyForNewChannels (acceptance #9) proves a route targeting an
// email notifier delivers via that notifier only (first-match-wins preserved) and
// that RouteTarget returns the notifier name so the hourly cap keys per channel.
func TestRouteAndThrottleKeyForNewChannels(t *testing.T) {
	st := openTestStore(t)
	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{
				Name: "ops-mail", Type: "email-smtp",
				SMTPHost: "smtp.example.com", SMTPPort: 587,
				From: "rabbot@example.com", To: []string{"team@example.com"},
			},
			{Name: "glue", Type: "generic-webhook", URL: "https://glue.example/hook"},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{"severity": "critical"}, Notifier: "ops-mail"},
			{Match: map[string]string{}, Notifier: "glue"}, // fallback
		},
		Alerting: config.AlertingConfig{DedupWindow: "5m", IncidentAutoCloseAfter: "24h"},
	}
	stack, err := BuildAlertingStack(cfg, st, http.DefaultClient, time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	crit := notify.Alert{Site: "example.com", ChangeType: "title", Severity: model.SeverityCritical}
	got := stack.Registry.Route(crit)
	if len(got) != 1 || got[0].Name() != "ops-mail" {
		t.Fatalf("critical alert routed to %v, want exactly [ops-mail]", names(got))
	}
	if target, ok := stack.Registry.RouteTarget(crit); !ok || target != "ops-mail" {
		t.Errorf("RouteTarget(critical) = %q,%v, want ops-mail,true (per-channel throttle key)", target, ok)
	}

	// A non-critical alert falls through to the generic-webhook fallback.
	warn := notify.Alert{Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning}
	got = stack.Registry.Route(warn)
	if len(got) != 1 || got[0].Name() != "glue" {
		t.Fatalf("warning alert routed to %v, want exactly [glue] (fallback)", names(got))
	}
	if target, ok := stack.Registry.RouteTarget(warn); !ok || target != "glue" {
		t.Errorf("RouteTarget(warning) = %q,%v, want glue,true", target, ok)
	}
}

// TestBuildAlertingStackDeliversByName (acceptance #11) is the end-to-end-by-name
// proof: a stack built from a multi-notifier config delivers a synthetic alert (the
// shape `rabbot notify test <name>` and the MCP send_test_alert produce) to the
// resolved endpoint via Registry.Get(name).Notify — the EXACT path both surfaces
// use — so the new types work there with zero CLI/MCP changes. The webhook and
// slack legs hit real httptest servers; routing is verified for the email leg.
func TestBuildAlertingStackDeliversByName(t *testing.T) {
	var slackHits, glueHits int64
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&slackHits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slackSrv.Close)
	glueSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&glueHits, 1)
		// Assert the generic-webhook contract reaches the endpoint by name.
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("webhook Content-Type = %q, want application/json", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer glue-secret" {
			t.Errorf("webhook Authorization = %q, want the configured header", auth)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(glueSrv.Close)

	st := openTestStore(t)
	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{Name: "slack", Type: "slack-webhook", URL: slackSrv.URL},
			{
				Name: "ops-mail", Type: "email-smtp",
				SMTPHost: "smtp.example.com", SMTPPort: 587,
				From: "rabbot@example.com", To: []string{"team@example.com"},
			},
			{
				Name: "glue", Type: "generic-webhook", URL: glueSrv.URL,
				Headers: map[string]string{"Authorization": "Bearer glue-secret"},
			},
		},
		Alerting: config.AlertingConfig{DedupWindow: "5m", IncidentAutoCloseAfter: "24h"},
	}
	stack, err := BuildAlertingStack(cfg, st, slackSrv.Client(), time.Now, nil, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	// The synthetic alert NotifyTest / send_test_alert build (run.go).
	sample := notify.Alert{
		Site: "rabbot-test.example", URL: "https://rabbot-test.example/",
		ChangeType: "notify_test", Severity: model.SeverityInfo,
		Before: "(test before)", After: "(test after)",
		DetectedAt: time.Now().UTC(), DeepLink: "https://rabbot-test.example/",
	}

	// Deliver to the webhook + slack legs by name (the daemon/MCP path), asserting
	// both reachable types fire.
	for _, name := range []string{"slack", "glue"} {
		n, ok := stack.Registry.Get(name)
		if !ok {
			t.Fatalf("Registry.Get(%q) not found", name)
		}
		if err := n.Notify(context.Background(), sample); err != nil {
			t.Fatalf("Notify via %q: %v", name, err)
		}
	}
	if got := atomic.LoadInt64(&slackHits); got != 1 {
		t.Errorf("slack endpoint hit %d times, want 1", got)
	}
	if got := atomic.LoadInt64(&glueHits); got != 1 {
		t.Errorf("generic-webhook endpoint hit %d times, want 1", got)
	}

	// The email leg is resolvable by name too (delivery itself is proven in
	// internal/notify against a fake SMTP server; here we prove by-name resolution).
	if _, ok := stack.Registry.Get("ops-mail"); !ok {
		t.Error("Registry.Get(\"ops-mail\") not found — email-smtp not reachable by name")
	}
}

// names is a test helper rendering notifier names for failure messages.
func names(ns []notify.Notifier) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name())
	}
	return out
}
