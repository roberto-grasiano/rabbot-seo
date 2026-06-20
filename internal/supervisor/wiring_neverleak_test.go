package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// secretSweep is the synchronized buffer the captured daemon log is written into,
// so the test goroutine and the slog handler do not race on the bytes (the test
// runs under -race).
type secretSweep struct {
	mu  sync.Mutex
	buf []byte
}

func (s *secretSweep) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *secretSweep) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

// TestAlertingE2E_NeverLeaksSecretsToLogsOrErrors is the integration-gate never-leak
// sweep the A1 spec demands: a stack is built from a config that NAMES a
// generic-webhook channel whose URL path AND a static Authorization header carry
// known secrets, plus an email-smtp channel carrying a known SMTP password. A real
// alert is then driven through the EXACT production delivery path
// (config → BuildAlertingStack → DigestFlush → dispatcher.Dispatch → the notifier),
// where the webhook endpoint FAILS so the daemon logs the delivery error via a real
// obs logger. The captured log AND the dispatched delivery's returned error are then
// swept for every secret fragment: none may appear. This proves the secret-scrubbing
// the email/webhook notifiers do holds end-to-end at the daemon's logging seam
// (wiring.go's "digest delivery failed" Error), not just in the notifiers' own unit
// tests.
func TestAlertingE2E_NeverLeaksSecretsToLogsOrErrors(t *testing.T) {
	const (
		urlPathSecret = "WEBHOOK-URL-PATH-SECRET-9KQ2"
		urlQuerySec   = "WEBHOOK-QUERY-SECRET-7ZX1"
		authSecret    = "Bearer WEBHOOK-AUTH-SECRET-4MN8"
		smtpPassword  = "SMTP-PASSWORD-SECRET-1AB3"
	)

	// A webhook endpoint that ALWAYS fails (500) so delivery exhausts retries and the
	// daemon logs the (scrubbed) error. Retry-After: 0 keeps the retry loop instant.
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// Build the webhook URL with a secret in BOTH the path and the query — the shapes
	// real targets (ntfy tokens, signed URLs) use.
	webhookURL := srv.URL + "/" + urlPathSecret + "?token=" + urlQuerySec

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Notifiers: []config.NotifierConfig{
			{
				Name: "glue", Type: config.NotifierTypeWebhook,
				URL:     webhookURL,
				Headers: map[string]string{"Authorization": authSecret},
			},
			{
				Name: "ops-mail", Type: config.NotifierTypeEmail,
				SMTPHost: "smtp.example.com", SMTPPort: 587,
				Username: "alerts@example.com", Password: smtpPassword,
				From: "rabbot@example.com", To: []string{"team@example.com"},
			},
		},
		Routes: []config.RouteConfig{
			{Match: map[string]string{}, Notifier: "glue"}, // catch-all -> failing webhook
		},
		Alerting: config.AlertingConfig{
			DedupWindow: "5m", PerRecipientHourlyCap: 1, IncidentAutoCloseAfter: "24h",
		},
	}

	// A REAL daemon logger writing JSON to a captured buffer — the same logger shape
	// runDaemon builds. DigestFlush logs delivery failures through it.
	sink := &secretSweep{}
	logger := obs.NewLogger(sink, "debug")

	stack, err := BuildAlertingStack(cfg, st, srv.Client(), time.Now, logger, nil)
	if err != nil {
		t.Fatalf("BuildAlertingStack: %v", err)
	}

	ctx := context.Background()
	siteID, err := st.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "example", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}

	// Drive two non-critical events to the SAME throttle recipient. With HourlyCap=1
	// the first is delivered live (fails, logged) and the second is over cap and
	// accrues to the digest buffer. Both delivery attempts hit the failing endpoint
	// and surface a scrubbed error that the daemon logs.
	first := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/a",
		ChangeType: "title", Severity: model.SeverityWarning, Before: "Old", After: "New",
	}
	// The pipeline returns the (scrubbed) delivery error for the live first send;
	// sweep THAT returned error too, not just the logs.
	liveErr := stack.Pipeline.Ingest(ctx, first)

	second := alerts.Event{
		SiteID: siteID, Site: "example.com", URL: "https://example.com/b",
		ChangeType: "meta_description", Severity: model.SeverityWarning, Before: "x", After: "y",
	}
	if err := stack.Pipeline.Ingest(ctx, second); err != nil {
		// An over-cap buffer Add returns nil; a non-nil here would be unexpected but is
		// still swept below via the logs.
		_ = err
	}

	// Flush the digest: the buffered (second) alert dispatches to the failing webhook,
	// and the daemon logs the scrubbed delivery error via the real logger.
	stack.DigestFlush(ctx)

	if atomic.LoadInt64(&hits) == 0 {
		t.Fatal("expected the webhook endpoint to be hit by the delivery path")
	}

	// THE SWEEP: neither the captured daemon logs nor the returned live error may
	// contain any secret fragment.
	logs := sink.String()
	if strings.TrimSpace(logs) == "" {
		t.Fatal("expected the daemon to log the (scrubbed) delivery failure; captured no logs")
	}
	var liveErrStr string
	if liveErr != nil {
		liveErrStr = liveErr.Error()
	}

	secrets := []string{urlPathSecret, urlQuerySec, "WEBHOOK-AUTH-SECRET-4MN8", smtpPassword}
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("daemon LOGS leaked secret %q:\n%s", secret, logs)
		}
		if liveErrStr != "" && strings.Contains(liveErrStr, secret) {
			t.Errorf("returned delivery error leaked secret %q: %s", secret, liveErrStr)
		}
	}
	// Sanity: the sweep is load-bearing only if a delivery error was actually logged.
	if !strings.Contains(logs, "digest delivery failed") {
		t.Errorf("expected a 'digest delivery failed' log line proving the error path ran:\n%s", logs)
	}
}
